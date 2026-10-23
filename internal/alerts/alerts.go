// Package alerts delivers alerts to webhooks: signed JSON, or Slack
// messages. Deciding when to raise an alert is the server's job; this
// package only formats and sends (docs/alerts.md).
package alerts

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// Ref names an organization, team or key in an alert.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// Event is an alert as webhooks receive it.
type Event struct {
	ID    string    `json:"id"`
	Event string    `json:"event"`
	Time  time.Time `json:"ts"`
	Org   *Ref      `json:"org,omitempty"`
	Team  *Ref      `json:"team,omitempty"`
	Key   *Ref      `json:"key,omitempty"`
	Data  any       `json:"data"`
	// Text is the one-line summary used for Slack.
	Text string `json:"-"`
}

// SignatureHeader carries the HMAC of the body when a webhook has a secret.
const SignatureHeader = "X-GOG-Signature"

// Sign returns the signature header value for body sent at t:
// "t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<body>">".
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// Body renders an event in a webhook's format.
func Body(format string, e *Event) ([]byte, error) {
	if format == config.FormatSlack {
		return json.Marshal(map[string]any{
			"text": e.Text,
			"blocks": []any{map[string]any{
				"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": e.Text},
			}},
		})
	}
	return json.Marshal(e)
}

// DefaultRetryDelays are the waits before each retry of a failed delivery.
var DefaultRetryDelays = []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}

// Notifier sends events to webhooks in the background.
type Notifier struct {
	log     *slog.Logger
	client  *http.Client
	agent   string
	delays  []time.Duration
	observe func(webhook, result string)

	queue chan delivery
	wg    sync.WaitGroup
}

type delivery struct {
	hook    config.Webhook
	event   *Event
	attempt int
}

// queueSize bounds deliveries waiting to be sent; more are dropped.
const queueSize = 1000

// New returns a notifier. transport may be nil for the default. observe,
// which may be nil, is told each delivery's result: ok, retried, failed
// or dropped.
func New(log *slog.Logger, transport http.RoundTripper, version string, observe func(webhook, result string)) *Notifier {
	if observe == nil {
		observe = func(string, string) {}
	}
	return &Notifier{
		log: log,
		client: &http.Client{
			Timeout: 10 * time.Second, Transport: transport,
			// A redirect could point anywhere; receivers must answer directly.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		agent:   "GenAIOpenGateway/" + version,
		delays:  DefaultRetryDelays,
		observe: observe,
		queue:   make(chan delivery, queueSize),
	}
}

// SetRetryDelays replaces the retry schedule, for tests.
func (n *Notifier) SetRetryDelays(d []time.Duration) { n.delays = d }

// Send queues an event for each webhook. It never blocks.
func (n *Notifier) Send(hooks []config.Webhook, e *Event) {
	for _, h := range hooks {
		n.enqueue(delivery{hook: h, event: e})
	}
}

func (n *Notifier) enqueue(d delivery) {
	select {
	case n.queue <- d:
	default:
		n.observe(d.hook.Name, "dropped")
		n.log.Warn("alert queue full; alert dropped", "webhook", d.hook.Name, "event", d.event.Event)
	}
}

// Run delivers queued events until ctx is done.
func (n *Notifier) Run(ctx context.Context) {
	const workers = 4
	for range workers {
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-n.queue:
					n.deliver(ctx, d)
				}
			}
		}()
	}
	n.wg.Wait()
}

func (n *Notifier) deliver(ctx context.Context, d delivery) {
	status, err := n.Deliver(ctx, d.hook, d.event)
	log := n.log.With("webhook", d.hook.Name, "event", d.event.Event, "alert_id", d.event.ID, "attempt", d.attempt+1)
	switch {
	case err == nil:
		n.observe(d.hook.Name, "ok")
		log.Info("alert delivered", "status", status)
	case retryable(status) && d.attempt < len(n.delays):
		n.observe(d.hook.Name, "retried")
		log.Warn("alert delivery failed; will retry", "status", status, "err", err)
		delay := n.delays[d.attempt]
		d.attempt++
		time.AfterFunc(delay, func() {
			if ctx.Err() == nil {
				n.enqueue(d)
			}
		})
	default:
		n.observe(d.hook.Name, "failed")
		log.Error("alert delivery failed", "status", status, "err", err)
	}
}

// retryable reports whether a failed attempt may succeed later: no
// response at all, 429 or 5xx.
func retryable(status int) bool {
	return status == 0 || status == http.StatusTooManyRequests || status >= 500
}

// Deliver makes one attempt to send an event to a webhook and returns the
// receiver's status code, or 0 when there was no response.
func (n *Notifier) Deliver(ctx context.Context, h config.Webhook, e *Event) (int, error) {
	body, err := Body(h.Format, e)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL.Value(), bytes.NewReader(body))
	if err != nil {
		// The error would print the URL, which can be a secret.
		return 0, errors.New("invalid webhook request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", n.agent)
	if s := h.Secret.Value(); s != "" {
		req.Header.Set(SignatureHeader, Sign(s, time.Now(), body))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // without the URL
		}
		return 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("receiver answered %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}
