package bedrock

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// streamingEvents is the payload hash of a request whose body is a
// signed event stream.
const streamingEvents = "STREAMING-AWS4-HMAC-SHA256-EVENTS"

// isSonic reports whether a model holds bidirectional speech sessions,
// such as amazon.nova-sonic-v1:0 or amazon.nova-2-sonic-v1:0.
func isSonic(model string) bool { return strings.Contains(model, "-sonic-") }

// OpenBidi implements provider.BidiOpener with
// InvokeModelWithBidirectionalStream. The request body is an event
// stream that stays open for the session: each input event is a chunk
// with its JSON in base64, and with SigV4 every message is wrapped in an
// envelope signed from the previous signature, as the AWS SDKs do. The
// call needs HTTP/2, which the shared client negotiates over TLS.
func (p *Provider) OpenBidi(ctx context.Context, model string, first []byte) (provider.Bidi, int, error) {
	ctx, cancel := context.WithCancel(ctx)
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url(model, "invoke-with-bidirectional-stream"), pr)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/vnd.amazon.eventstream")
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	b := &bidi{body: pw, cancel: cancel, enc: eventstream.NewEncoder()}
	if err := p.auth.authorizeStream(req, b); err != nil {
		cancel()
		return nil, http.StatusBadGateway, fmt.Errorf("upstream credentials: %w", err)
	}
	// The first event goes out while the client waits for the response,
	// since the upstream may answer only once input arrives.
	go func() {
		if err := b.Send(first); err != nil {
			_ = pw.CloseWithError(err)
		}
	}()
	dctx, dcancel := context.WithTimeout(ctx, p.cfg.Timeout.Std())
	defer dcancel()
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := p.client.Do(req)
		done <- result{resp, err}
	}()
	var res result
	select {
	case res = <-done:
	case <-dctx.Done():
		cancel()
		_ = pw.Close()
		if r := <-done; r.resp != nil {
			_ = r.resp.Body.Close()
		}
		return nil, http.StatusGatewayTimeout, errors.New("the upstream did not answer")
	}
	if res.err != nil {
		cancel()
		_ = pw.Close()
		return nil, 0, res.err
	}
	if res.resp.StatusCode != http.StatusOK {
		// Read the error before cancelling, which would cut the body short.
		e := readError(&provider.Response{StatusCode: res.resp.StatusCode, Header: res.resp.Header, Body: res.resp.Body})
		cancel()
		_ = pw.Close()
		return nil, e.status, fmt.Errorf("%s: %s", e.typ, e.message)
	}
	b.resp = res.resp.Body
	b.dec = eventstream.NewDecoder()
	return b, http.StatusOK, nil
}

// authorizeStream authorizes a bidirectional stream: with an API key as
// any request, with SigV4 by signing the headers for a streamed body
// and keeping the signature to chain the messages from.
func (s *signer) authorizeStream(r *http.Request, b *bidi) error {
	if s.apiKey != "" {
		return s.authorize(r)
	}
	r.Header.Set("Accept", "application/vnd.amazon.eventstream")
	creds, err := s.credentials(r.Context())
	if err != nil {
		return err
	}
	c, err := creds.Retrieve(r.Context())
	if err != nil {
		return err
	}
	r.Header.Set("X-Amz-Content-Sha256", streamingEvents)
	if err := s.v4.SignHTTP(r.Context(), c, r, streamingEvents, "bedrock", s.region, time.Now()); err != nil {
		return err
	}
	_, sig, ok := strings.Cut(r.Header.Get("Authorization"), "Signature=")
	if !ok {
		return errors.New("signing the stream: no seed signature")
	}
	seed, err := hex.DecodeString(sig)
	if err != nil {
		return err
	}
	b.signer = v4.NewStreamSigner(c, "bedrock", s.region, seed)
	return nil
}

// bidi is an open bidirectional stream.
type bidi struct {
	mu     sync.Mutex
	body   *io.PipeWriter
	enc    *eventstream.Encoder
	signer *v4.StreamSigner
	closed bool

	resp   io.ReadCloser
	dec    *eventstream.Decoder
	buf    []byte
	cancel context.CancelFunc
}

// Send implements provider.Bidi.
func (b *bidi) Send(event []byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return io.ErrClosedPipe
	}
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString(event)})
	var m eventstream.Message
	m.Headers.Set(":message-type", eventstream.StringValue("event"))
	m.Headers.Set(":event-type", eventstream.StringValue("chunk"))
	m.Headers.Set(":content-type", eventstream.StringValue("application/json"))
	m.Payload = payload
	return b.write(m)
}

// write sends a message, inside a signed envelope with SigV4. The
// caller holds mu.
func (b *bidi) write(m eventstream.Message) error {
	var frame bytes.Buffer
	if err := b.enc.Encode(&frame, m); err != nil {
		return err
	}
	if b.signer != nil {
		var err error
		if m, err = b.envelope(frame.Bytes()); err != nil {
			return err
		}
		frame.Reset()
		if err := b.enc.Encode(&frame, m); err != nil {
			return err
		}
	}
	_, err := b.body.Write(frame.Bytes())
	return err
}

// envelope wraps an encoded message, or nothing at the end of the
// stream, with the :date and :chunk-signature headers.
func (b *bidi) envelope(payload []byte) (eventstream.Message, error) {
	now := time.Now().UTC()
	var m eventstream.Message
	m.Headers.Set(":date", eventstream.TimestampValue(now))
	var headers bytes.Buffer
	if err := eventstream.EncodeHeaders(&headers, m.Headers); err != nil {
		return m, err
	}
	sig, err := b.signer.GetSignature(context.Background(), headers.Bytes(), payload, now)
	if err != nil {
		return m, err
	}
	m.Headers.Set(":chunk-signature", eventstream.BytesValue(sig))
	m.Payload = payload
	return m, nil
}

// CloseSend implements provider.Bidi: with SigV4 an empty signed
// message marks the end of the input.
func (b *bidi) CloseSend() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	if b.signer != nil {
		if m, err := b.envelope(nil); err == nil {
			var frame bytes.Buffer
			if b.enc.Encode(&frame, m) == nil {
				_, _ = b.body.Write(frame.Bytes())
			}
		}
	}
	return b.body.Close()
}

// Recv implements provider.Bidi: the JSON of the next chunk. An
// exception ends the stream with its message.
func (b *bidi) Recv() ([]byte, error) {
	for {
		m, err := b.dec.Decode(b.resp, b.buf)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				err = io.EOF
			}
			return nil, err
		}
		if e, ok := frameError(m); ok {
			return nil, fmt.Errorf("%s: %s", e.typ, e.message)
		}
		if header(m, ":event-type") != "chunk" {
			continue
		}
		var chunk struct {
			Bytes string `json:"bytes"`
		}
		if json.Unmarshal(m.Payload, &chunk) != nil {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(chunk.Bytes)
		if err != nil {
			continue
		}
		return data, nil
	}
}

// Close implements provider.Bidi.
func (b *bidi) Close() error {
	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()
	b.cancel()
	_ = b.body.Close()
	if b.resp != nil {
		return b.resp.Close()
	}
	return nil
}
