package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Push notifications (docs/a2a.md#push-notifications). An agent that
// sends them calls a URL its caller chose, which through a gateway
// would let any caller make the agent call anything the agent can
// reach. So the gateway stands between them: the agent is given a URL
// at the gateway that ends in a secret, and the gateway delivers what
// arrives there to the caller's URL, after checking where that is.

const (
	// a2aPushTTL is how long a configuration is honored. Tasks that run
	// longer set a new one.
	a2aPushTTL = 30 * 24 * time.Hour
	// a2aPushTimeout bounds one delivery to a caller's webhook.
	a2aPushTimeout = 15 * time.Second
	// maxA2APush bounds a notification.
	maxA2APush = 8 << 20
)

// pushSecret is what a caller's configuration holds besides the URL.
type pushSecret struct {
	Token          string          `json:"token,omitempty"`
	Authentication json.RawMessage `json:"authentication,omitempty"`
}

func pushAD(agent, id string) []byte { return []byte("a2a-push/" + agent + "/" + id) }

// pushConfigIn finds the push notification configuration in a request:
// under the name the protocol version and binding give it, or the
// request itself, as in A2A 1.0.
func pushConfigIn(m map[string]any) map[string]any {
	for _, k := range []string{"pushNotificationConfig", "pushNotification", "taskPushNotificationConfig"} {
		if o, _ := m[k].(map[string]any); o != nil {
			return o
		}
	}
	if _, ok := m["url"].(string); ok {
		return m
	}
	return nil
}

// publicIP reports whether an address is on the public internet.
func publicIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
		// Shared address space (RFC 6598) is as internal as RFC 1918.
		if v4[0] == 100 && v4[1]&0xc0 == 64 {
			return false
		}
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast())
}

// hostAllowed reports whether host matches a pattern: a host name, or
// "*." and a domain for its subdomains.
func hostAllowed(patterns []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, p := range patterns {
		p = strings.ToLower(p)
		if p == host || (strings.HasPrefix(p, "*.") && strings.HasSuffix(host, p[1:])) {
			return true
		}
	}
	return false
}

// checkPushURL says why a caller's webhook URL is not accepted, or "".
// A host the configuration names may be anywhere; any other must use
// HTTPS and, when it is delivered to, resolve to public addresses.
func checkPushURL(cfg config.A2APush, raw string) (trusted bool, why string) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return false, "the push notification URL must be an http or https URL without credentials"
	}
	host := u.Hostname()
	if hostAllowed(cfg.AllowedHosts, host) {
		return true, ""
	}
	switch {
	case len(cfg.AllowedHosts) > 0:
		return false, "the push notification URL's host is not one this gateway delivers to"
	case u.Scheme != "https":
		return false, "the push notification URL must use https"
	}
	if ip := net.ParseIP(host); ip != nil && !publicIP(ip) {
		return false, "the push notification URL must not be an internal address"
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return false, "the push notification URL must not be an internal address"
	}
	return false, ""
}

// newPublicClient returns a client that connects only to public
// addresses, whatever a name resolves to, and follows no redirects.
func newPublicClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		if ip := net.ParseIP(host); ip == nil || !publicIP(ip) {
			return errors.New("the address is not public")
		}
		return nil
	}}
	return &http.Client{
		Transport:     &http.Transport{DialContext: d.DialContext, TLSHandshakeTimeout: 10 * time.Second, MaxIdleConns: 20, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// pushBase is the gateway's URL as agents reach it, or "".
func pushBase(cfg *config.Config) string {
	if cfg.A2A.PublicURL != "" {
		return cfg.A2A.PublicURL
	}
	return strings.TrimRight(cfg.MCP.PublicURL, "/")
}

// setPush replaces the caller's webhook in a push configuration with
// the gateway's, and remembers the caller's. It returns false after
// writing an error.
func (c *a2aCall) setPush(w http.ResponseWriter, ctx context.Context, conf map[string]any, taskID string) bool {
	cfg := c.rt.cfg
	if !cfg.A2A.Push.Enabled || c.s.store == nil {
		c.rpcError(w, a2aPushNotSupported, "invalid", "push notifications are not available through the gateway")
		return false
	}
	target, _ := conf["url"].(string)
	if _, why := checkPushURL(cfg.A2A.Push, target); why != "" {
		c.rpcError(w, rpcInvalidParams, "invalid", why)
		return false
	}
	row := &store.A2APush{Agent: c.name, TaskID: taskID, OrgID: c.p.OrgID, TeamID: c.p.TeamID, KeyID: c.p.KeyID, URL: target, ExpiresAt: time.Now().Add(a2aPushTTL)}
	row.ConfigID, _ = conf["id"].(string)
	secret := randomString()
	row.ID = hashID(secret)
	var ps pushSecret
	ps.Token, _ = conf["token"].(string)
	if a := conf["authentication"]; a != nil {
		ps.Authentication, _ = json.Marshal(a)
	}
	if ps.Token != "" || ps.Authentication != nil {
		box, err := secretBox(cfg.Secrets)
		if err != nil {
			c.rpcError(w, a2aPushNotSupported, "invalid", "push notifications with a token or credentials need secrets.encryption_key at the gateway")
			return false
		}
		raw, _ := json.Marshal(ps)
		row.Secret = box.Seal(raw, pushAD(c.name, row.ID))
	}
	if err := c.s.store.InsertA2APush(ctx, row); err != nil {
		c.s.log.Error("storing an A2A push configuration failed", "request_id", c.rid, "err", err)
		c.rpcError(w, rpcInternalError, "error", "the gateway could not store the push notification configuration")
		return false
	}
	conf["url"] = pushBase(cfg) + "/a2a/" + c.name + "/push/" + secret
	// The agent authenticates to the gateway with the URL; the caller's
	// token and credentials are for the gateway's delivery.
	delete(conf, "token")
	delete(conf, "authentication")
	return true
}

// restorePush puts the caller's own webhook URLs back into an agent's
// answer about push configurations, and learns the ids the agent gave
// them. A URL of another caller's configuration is blanked.
func (c *a2aCall) restorePush(ctx context.Context, v any, taskID string) {
	prefix := pushBase(c.rt.cfg) + "/a2a/" + c.name + "/push/"
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if u, _ := v["url"].(string); strings.HasPrefix(u, prefix) {
				v["url"] = ""
				row, err := c.s.store.GetA2APush(ctx, hashID(strings.TrimPrefix(u, prefix)), time.Now())
				if err == nil && row.OrgID == c.p.OrgID && row.TeamID == c.p.TeamID && (c.p.TeamID != auth.StaticTeamID || row.KeyID == c.p.KeyID) {
					v["url"] = row.URL
					if id, _ := v["id"].(string); (row.ConfigID == "" && id != "") || (row.TaskID == "" && taskID != "") {
						_ = c.s.store.NameA2APush(ctx, row.ID, taskID, id)
					}
				}
			}
			for _, x := range v {
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(v)
}

// notifiedTask finds the task a notification is about: a task itself,
// or an update or message that names one, in the shapes of A2A 0.3 and
// 1.0.
func notifiedTask(n map[string]any) string {
	if t, _ := n["task"].(map[string]any); t != nil {
		id, _ := t["id"].(string)
		return id
	}
	for _, k := range []string{"statusUpdate", "artifactUpdate", "message"} {
		if m, _ := n[k].(map[string]any); m != nil {
			id, _ := m["taskId"].(string)
			return id
		}
	}
	if id, _ := n["taskId"].(string); id != "" {
		return id
	}
	id, _ := n["id"].(string)
	return id
}

// a2aPushNotify takes a notification from an agent and delivers it to
// the webhook of the caller that set the configuration.
func (s *Server) a2aPushNotify(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rt := s.rt.Load()
	name := r.PathValue("name")
	status := "ok"
	defer func() { s.metrics.ObserveA2ARequest(name, "push/notify", status, time.Since(start)) }()
	refuse := func(code int, st, msg string) {
		status = st
		http.Error(w, msg, code)
	}
	agent, ok := rt.cfg.A2A.Agents[name]
	if !ok || !rt.cfg.A2A.Push.Enabled || s.store == nil {
		refuse(http.StatusNotFound, "invalid", "not found")
		return
	}
	row, err := s.store.GetA2APush(r.Context(), hashID(r.PathValue("secret")), time.Now())
	if err != nil || row.Agent != name {
		refuse(http.StatusNotFound, "not_found", "not found")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxA2APush))
	var n map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err != nil || dec.Decode(&n) != nil || n == nil {
		refuse(http.StatusBadRequest, "invalid", "a notification must be a JSON object")
		return
	}
	// A configuration is for one task. One set with a message learns
	// its task from the first notification.
	task := notifiedTask(n)
	switch {
	case task == "":
		refuse(http.StatusBadRequest, "invalid", "the notification names no task")
		return
	case row.TaskID != "" && row.TaskID != task:
		refuse(http.StatusForbidden, "denied", "this URL is for another task")
		return
	case row.TaskID == "":
		o, err := s.store.GetA2AObject(r.Context(), name, store.A2ATask, task)
		if err == nil && (o.OrgID != row.OrgID || o.TeamID != row.TeamID || (row.TeamID == auth.StaticTeamID && o.KeyID != row.KeyID)) {
			refuse(http.StatusForbidden, "denied", "this URL is for another task")
			return
		}
		_ = s.store.RecordA2AObject(r.Context(), &store.A2AObject{Agent: name, Kind: store.A2ATask, ID: task, OrgID: row.OrgID, TeamID: row.TeamID, KeyID: row.KeyID})
		_ = s.store.NameA2APush(r.Context(), row.ID, task, "")
	}
	p := &auth.Principal{OrgID: row.OrgID, TeamID: row.TeamID, KeyID: row.KeyID}
	rid := r.Header.Get("X-Request-Id")
	if rt.guards.Applies(config.StageOutput, agent.Guardrails) {
		parts := textParts(n)
		texts := make([]string, len(parts))
		for i, part := range parts {
			texts[i] = part["text"].(string)
		}
		meta := guardMeta(rid, p)
		res := rt.guards.Run(r.Context(), config.StageOutput, agent.Guardrails, texts, meta)
		s.recordGuardrails(meta, res.Decisions)
		if res.Blocked != nil {
			// The agent did its part; the caller reads the task instead.
			status = "guardrail_blocked"
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if res.Changed {
			for i, part := range parts {
				part["text"] = res.Texts[i]
			}
			raw, _ = json.Marshal(n)
		}
	}
	code, err := s.deliverPush(r.Context(), rt.cfg, row, raw, r.Header.Get("Content-Type"))
	s.log.Info("a2a push notification", "request_id", rid, "agent", name, "task", task, "org_id", row.OrgID, "team_id", row.TeamID, "key_id", row.KeyID,
		"webhook_status", code, "err", err, "duration_ms", time.Since(start).Milliseconds())
	if err != nil || code < 200 || code > 299 {
		refuse(http.StatusBadGateway, "upstream_error", "the notification could not be delivered")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deliverPush posts a notification to the caller's webhook, with the
// token and credentials the caller set.
func (s *Server) deliverPush(ctx context.Context, cfg *config.Config, row *store.A2APush, body []byte, contentType string) (int, error) {
	trusted, why := checkPushURL(cfg.A2A.Push, row.URL)
	if why != "" {
		return 0, errors.New(why)
	}
	ctx, cancel := context.WithTimeout(ctx, a2aPushTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, row.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if contentType == "" {
		contentType = "application/json"
	}
	req.Header.Set("Content-Type", contentType)
	if row.Secret != "" {
		box, err := secretBox(cfg.Secrets)
		if err != nil {
			return 0, err
		}
		plain, err := box.Open(row.Secret, pushAD(row.Agent, row.ID))
		if err != nil {
			return 0, fmt.Errorf("opening the configuration's secret: %w", err)
		}
		var ps pushSecret
		if err := json.Unmarshal(plain, &ps); err != nil {
			return 0, err
		}
		if ps.Token != "" {
			req.Header.Set("X-A2A-Notification-Token", ps.Token)
		}
		// A2A 1.0 names one scheme; 0.3 lists the schemes the webhook takes.
		var a struct {
			Scheme      string   `json:"scheme"`
			Schemes     []string `json:"schemes"`
			Credentials string   `json:"credentials"`
		}
		if json.Unmarshal(ps.Authentication, &a) == nil && a.Credentials != "" {
			if a.Scheme == "" && len(a.Schemes) > 0 {
				a.Scheme = a.Schemes[0]
			}
			if a.Scheme != "" {
				req.Header.Set("Authorization", a.Scheme+" "+a.Credentials)
			}
		}
	}
	client := s.pushClient
	if trusted {
		client = s.pushTrusted
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}
