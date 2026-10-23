package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/bodylog"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/mcpgw"
	"github.com/rnikadi/genaiopengateway/internal/redact"
	"github.com/rnikadi/genaiopengateway/internal/secretbox"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Body logging (docs/body-logging.md): handlers capture bodies of the
// requests whose policy asks for it, and a writer redacts, encrypts and
// stores them off the request path.

// bodyQueue bounds queued bodies. Each holds up to 4×max_body_bytes of
// request and of response before redaction, so the queue is far shorter
// than the usage queue, and bodies are dropped before usage records.
const bodyQueue = 256

// bodyLogger is the body logging configuration in effect.
type bodyLogger struct {
	redactor *redact.Redactor
	box      *secretbox.Box
	max      int
}

// newBodyLogger returns nil when body logging is off.
func newBodyLogger(cfg *config.Config) (*bodyLogger, error) {
	bl := cfg.BodyLogging
	if !bl.Enabled {
		return nil, nil
	}
	r, err := redact.New(redact.Config{Builtin: bl.Redact.Builtin, Patterns: bl.Redact.Patterns})
	if err != nil {
		return nil, err
	}
	box, err := secretBox(cfg.Secrets)
	if err != nil {
		return nil, err
	}
	return &bodyLogger{redactor: r, box: box, max: bl.MaxBodyBytes}, nil
}

// secretBox builds the box that encrypts values at rest.
func secretBox(sc config.Secrets) (*secretbox.Box, error) {
	current, err := secretbox.ParseKey(sc.EncryptionKey.Value())
	if err != nil {
		return nil, fmt.Errorf("secrets.encryption_key: %w", err)
	}
	var previous [][]byte
	for _, k := range sc.PreviousKeys {
		p, err := secretbox.ParseKey(k.Value())
		if err != nil {
			return nil, fmt.Errorf("secrets.previous_keys: %w", err)
		}
		previous = append(previous, p)
	}
	return secretbox.New(current, previous...)
}

// capturedBody is a body as captured, before redaction.
type capturedBody struct {
	store.RequestBody
	request, response   []byte
	responseContentType string
}

// logBodies reports whether a principal's requests are logged now.
func (s *Server) logBodies(p *auth.Principal) bool {
	return s.bodies != nil && s.rt.Load().bodies != nil && p.LogBodies()
}

// writeBodies is the sink of the body writer: it redacts, encrypts and
// stores a batch.
func (s *Server) writeBodies(ctx context.Context, batch []capturedBody) error {
	bl := s.rt.Load().bodies
	if bl == nil {
		return nil // turned off since the bodies were captured
	}
	out := make([]*store.RequestBody, 0, len(batch))
	for i := range batch {
		c := &batch[i]
		resp := c.response
		if c.Kind == "model" {
			resp = bodylog.Assemble(c.responseContentType, resp)
		}
		req, counts, truncReq := bl.redactor.Body(c.request, bl.max)
		resp, respCounts, truncResp := bl.redactor.Body(resp, bl.max)
		for k, n := range respCounts {
			counts[k] += n
		}
		s.metrics.ObserveRedactions(counts)
		rb := c.RequestBody
		rb.Truncated = rb.Truncated || truncReq || truncResp
		rb.Redactions, _ = json.Marshal(counts)
		rb.Request = bl.box.Seal(req, bodyAD(rb.RequestID, "request"))
		rb.Response = bl.box.Seal(resp, bodyAD(rb.RequestID, "response"))
		out = append(out, &rb)
	}
	if err := s.store.InsertRequestBodies(ctx, out); err != nil {
		return err
	}
	s.metrics.ObserveBodies(len(out))
	return nil
}

// bodyAD binds a sealed body to its request and field, so a ciphertext
// copied to another row does not decrypt.
func bodyAD(requestID, field string) []byte {
	return []byte("request_body:" + requestID + ":" + field)
}

func (s *Server) bodiesDropped() float64 {
	if s.bodies == nil {
		return 0
	}
	return float64(s.bodies.Dropped())
}

// captureBuffer keeps the first max bytes written to it.
type captureBuffer struct {
	bytes.Buffer
	max       int
	truncated bool
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); room < len(p) {
		b.truncated = true
		b.Buffer.Write(p[:max(room, 0)])
	} else {
		b.Buffer.Write(p)
	}
	return len(p), nil
}

// newCapture returns a buffer for a response body, or nil when the
// principal's bodies are not logged.
func (s *Server) newCapture(p *auth.Principal) *captureBuffer {
	if !s.logBodies(p) {
		return nil
	}
	// Redaction reads at most 4×max; see redact.Body.
	return &captureBuffer{max: 4 * s.rt.Load().bodies.max}
}

// logMCPBody is the MCP gateway's LogBody hook.
func (s *Server) logMCPBody(caller *mcpgw.Caller, vs, method string, params any, res mcp.Result, callErr error) {
	p := caller.Principal
	if !s.logBodies(p) {
		return
	}
	req, _ := json.Marshal(map[string]any{"method": method, "params": params})
	var resp []byte
	if callErr != nil {
		resp, _ = json.Marshal(map[string]any{"error": callErr.Error()})
	} else {
		resp, _ = json.Marshal(res)
	}
	s.bodies.Add(capturedBody{
		RequestBody: store.RequestBody{
			RequestID: caller.RequestID, Time: time.Now().UTC(), OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID,
			Kind: "mcp", Endpoint: "/mcp/" + vs,
		},
		request: req, response: resp,
	})
}

// --- admin API ---

func (s *Server) listRequests(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := s.readScope(r, q.Get("org_id"), q.Get("team_id"), q.Get("key_id")); err != nil {
		return err
	}
	rq := store.RequestQuery{OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), KeyID: q.Get("key_id")}
	for name, dst := range map[string]*time.Time{"from": &rq.From, "to": &rq.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return badRequest("%s must be an RFC 3339 time", name)
			}
			*dst = t
		}
	}
	items, more, err := s.store.ListRequests(r.Context(), rq, pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// requestBodyView is a decrypted body record as the admin API returns it.
type requestBodyView struct {
	RequestID  string          `json:"request_id"`
	Time       time.Time       `json:"ts"`
	OrgID      string          `json:"org_id"`
	TeamID     string          `json:"team_id"`
	KeyID      string          `json:"key_id"`
	Kind       string          `json:"kind"`
	Endpoint   string          `json:"endpoint"`
	Request    json.RawMessage `json:"request"`
	Response   json.RawMessage `json:"response"`
	Truncated  bool            `json:"truncated"`
	Redactions json.RawMessage `json:"redactions"`
}

// getRequestBody returns a request's logged body. Reading content is
// audited, and only owners and the organization's admins may.
func (s *Server) getRequestBody(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	bodies, err := s.store.RequestBodies(ctx, r.PathValue("request_id"))
	if err != nil {
		return err
	}
	op := operatorFrom(ctx)
	var b *store.RequestBody
	for _, x := range bodies {
		if op.canRead(x.OrgID) {
			b = x
			break
		}
	}
	if b == nil {
		return store.ErrNotFound // do not confirm another organization's request exists
	}
	if !op.canWrite(b.OrgID) {
		return &apiError{http.StatusForbidden, "forbidden", "reading request bodies needs the owner or admin role"}
	}
	box, err := secretBox(s.rt.Load().cfg.Secrets)
	if err != nil {
		return &apiError{http.StatusConflict, "no_encryption_key", "secrets.encryption_key is not configured, so bodies cannot be decrypted"}
	}
	view := requestBodyView{
		RequestID: b.RequestID, Time: b.Time, OrgID: b.OrgID, TeamID: b.TeamID, KeyID: b.KeyID,
		Kind: b.Kind, Endpoint: b.Endpoint, Truncated: b.Truncated, Redactions: b.Redactions,
	}
	for field, dst := range map[string]*json.RawMessage{"request": &view.Request, "response": &view.Response} {
		sealed := b.Request
		if field == "response" {
			sealed = b.Response
		}
		plain, err := box.Open(sealed, bodyAD(b.RequestID, field))
		if err != nil {
			return &apiError{http.StatusConflict, "undecryptable", "the body was encrypted with a key that is no longer configured"}
		}
		*dst = bodyJSON(plain)
	}
	if err := s.store.AppendAudit(ctx, &store.AuditEntry{
		Actor: op.actor(), Action: "request_body.read", ResourceType: "request", ResourceID: b.RequestID, OrgID: b.OrgID,
	}); err != nil {
		return err // no read without its audit record
	}
	writeJSON(w, http.StatusOK, view)
	return nil
}

// bodyJSON returns a stored body as JSON: itself when it is valid JSON,
// otherwise as a string, which is what a truncated or non-JSON body is.
func bodyJSON(b []byte) json.RawMessage {
	if json.Valid(b) {
		return b
	}
	s, _ := json.Marshal(string(b))
	return s
}

// deleteRequestBodies deletes an organization's bodies before a time.
func (s *Server) deleteRequestBodies(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	q := r.URL.Query()
	orgID := q.Get("org_id")
	if orgID == "" {
		return badRequest("org_id is required")
	}
	before, err := time.Parse(time.RFC3339, q.Get("before"))
	if err != nil {
		return badRequest("before must be an RFC 3339 time")
	}
	n, err := s.store.DeleteRequestBodies(r.Context(), orgID, before)
	if err != nil {
		return err
	}
	if err := s.audit(r.Context(), "request_body.delete", "org", orgID, orgID, map[string]any{"before": before, "deleted": n}); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	return nil
}

// bodyPruneLoop deletes bodies older than the retention period, hourly.
// It runs when body logging is off too, so bodies logged before it was
// turned off still expire.
func (s *Server) bodyPruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if retention := s.rt.Load().cfg.BodyLogging.Retention.Std(); retention > 0 {
			n, err := s.store.PruneRequestBodies(ctx, time.Now().Add(-retention))
			if err != nil {
				s.log.Error("pruning request bodies failed", "err", err)
			} else if n > 0 {
				s.log.Info("pruned request bodies", "rows", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
