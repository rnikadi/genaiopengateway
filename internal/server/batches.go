package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/auth"
	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/telemetry"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// Batch refresh: how often the loop runs, how many batches one run
// refreshes, and how stale an unfinished batch may be before it is
// refreshed (a variable for tests).
const (
	batchPollEvery = time.Minute
	batchPollMax   = 100
)

var batchStale = 5 * time.Minute

// batchURLs are the endpoints a batch input file may call.
var batchURLs = map[string]bool{
	"/v1/chat/completions": true, "/v1/responses": true, "/v1/embeddings": true, "/v1/completions": true, "/v1/moderations": true,
}

var (
	errFileNotFound  = errors.New("file not found")
	errBatchNotFound = errors.New("batch not found")
)

// ownsRecord reports whether a caller owns a file or batch: same
// organization and team, and for static keys the same key.
func ownsRecord(orgID, teamID, keyID string, p *auth.Principal) bool {
	return orgID == p.OrgID && teamID == p.TeamID && (p.TeamID != auth.StaticTeamID || keyID == p.KeyID)
}

// ownerKey is the key a static caller's lists are narrowed to.
func ownerKey(p *auth.Principal) string {
	if p.TeamID == auth.StaticTeamID {
		return p.KeyID
	}
	return ""
}

// ownedFile loads the file named in the path, if the caller owns it and
// may use its model.
func (s *Server) ownedFile(ctx context.Context, id string, p *auth.Principal) (*store.BatchFile, error) {
	f, err := s.store.GetBatchFile(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!ownsRecord(f.OrgID, f.TeamID, f.KeyID, p) || !p.AllowsModel(f.Model))) {
		return nil, errFileNotFound
	}
	return f, err
}

// ownedBatch loads the batch named in the path, if the caller owns it
// and may use its model.
func (s *Server) ownedBatch(ctx context.Context, id string, p *auth.Principal) (*store.Batch, error) {
	b, err := s.store.GetBatch(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (!ownsRecord(b.OrgID, b.TeamID, b.KeyID, p) || !p.AllowsModel(b.Model))) {
		return nil, errBatchNotFound
	}
	return b, err
}

func writeRecordError(w http.ResponseWriter, err error) {
	if errors.Is(err, errFileNotFound) || errors.Is(err, errBatchNotFound) {
		writeError(w, provider.Batches, http.StatusNotFound, codeNotFound, err.Error())
		return
	}
	writeError(w, provider.Batches, http.StatusInternalServerError, codeInvalidRequest, "the record could not be read")
}

// batchLine is one request of a batch input file.
type batchLine struct {
	CustomID string                     `json:"custom_id"`
	Method   string                     `json:"method"`
	URL      string                     `json:"url"`
	Body     map[string]json.RawMessage `json:"body"`
}

// batchInput is a checked batch input file.
type batchInput struct {
	model, url string
	lines      []batchLine
}

// parseBatchInput checks every line of a JSONL batch file: one model
// and one endpoint for all of them, as OpenAI requires.
func parseBatchInput(data []byte) (*batchInput, error) {
	in := &batchInput{}
	for i, raw := range bytes.Split(data, []byte("\n")) {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			continue
		}
		var l batchLine
		if err := json.Unmarshal(raw, &l); err != nil || l.Body == nil {
			return nil, fmt.Errorf("line %d: not a JSON object with a body", i+1)
		}
		var model string
		if json.Unmarshal(l.Body["model"], &model) != nil || model == "" {
			return nil, fmt.Errorf("line %d (%s): body.model is required", i+1, l.CustomID)
		}
		switch {
		case l.CustomID == "":
			return nil, fmt.Errorf("line %d: custom_id is required", i+1)
		case l.Method != http.MethodPost:
			return nil, fmt.Errorf("line %d (%s): method must be POST", i+1, l.CustomID)
		case !batchURLs[l.URL]:
			return nil, fmt.Errorf("line %d (%s): url %q is not served in batches", i+1, l.CustomID, l.URL)
		case in.model != "" && model != in.model:
			return nil, fmt.Errorf("line %d (%s): every line must use model %s", i+1, l.CustomID, in.model)
		case in.url != "" && l.URL != in.url:
			return nil, fmt.Errorf("line %d (%s): every line must use url %s", i+1, l.CustomID, in.url)
		}
		in.model, in.url = model, l.URL
		in.lines = append(in.lines, l)
	}
	if len(in.lines) == 0 {
		return nil, errors.New("the file has no requests")
	}
	return in, nil
}

// writeTo writes the lines as JSONL with the model rewritten to an
// upstream model.
func (in *batchInput) writeTo(w io.Writer, upstreamModel string) error {
	model, _ := json.Marshal(upstreamModel)
	for _, l := range in.lines {
		body := make(map[string]json.RawMessage, len(l.Body))
		for k, v := range l.Body {
			body[k] = v
		}
		body["model"] = model
		l.Body = body
		raw, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if _, err := w.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// uploadFile checks a batch input file and uploads it to the first
// target of its model that accepts it.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	p, ok := s.videoCaller(w, r)
	if !ok {
		return
	}
	fail := func(status int, code, msg string) { writeError(w, provider.Batches, status, code, msg) }
	rt := s.rt.Load()
	if !isMultipart(r) {
		fail(http.StatusBadRequest, codeInvalidRequest, "upload the file as multipart/form-data")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, rt.cfg.Limits.MaxBatchFileBytes+1<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(http.StatusRequestEntityTooLarge, codeTooLarge, "the file is too large")
		} else {
			fail(http.StatusBadRequest, codeInvalidRequest, "could not read request body")
		}
		return
	}
	fields, files, err := readMultipart(r, raw)
	if err != nil {
		fail(http.StatusBadRequest, codeInvalidRequest, "could not read the multipart body: "+err.Error())
		return
	}
	var purpose string
	_ = json.Unmarshal(fields["purpose"], &purpose)
	if purpose != "batch" {
		fail(http.StatusBadRequest, codeInvalidRequest, `only purpose "batch" is served`)
		return
	}
	var file *provider.File
	for i := range files {
		if files[i].Field == "file" {
			file = &files[i]
		}
	}
	if file == nil {
		fail(http.StatusBadRequest, codeInvalidRequest, "file is required")
		return
	}
	if int64(len(file.Data)) > rt.cfg.Limits.MaxBatchFileBytes {
		fail(http.StatusRequestEntityTooLarge, codeTooLarge, "the file is too large")
		return
	}
	in, err := parseBatchInput(file.Data)
	if err != nil {
		fail(http.StatusBadRequest, codeInvalidRequest, err.Error())
		return
	}
	if !p.AllowsModel(in.model) {
		fail(http.StatusForbidden, codeModelNotAllowed, "this key may not use model "+in.model)
		return
	}
	names := p.Guardrails()
	if rt.guards.Applies(config.StageOutput, names) {
		fail(http.StatusBadRequest, codeInvalidRequest, "batches are not available when output guardrails apply: their results are not checked")
		return
	}
	requestID := r.Header.Get("X-Request-Id")
	if status, code, msg, ok := s.admit(w, r, p, requestID); !ok {
		fail(status, code, msg)
		return
	}
	meta := guardMeta(requestID, p)
	for i := range in.lines {
		l := &in.lines[i]
		body, _ := json.Marshal(l.Body)
		sent, res := s.guardRequest(r.Context(), rt, meta, names, body)
		if res.Blocked != nil {
			setGuardrailsHeader(w, res.Decisions)
			fail(http.StatusBadRequest, codeGuardrailBlocked, fmt.Sprintf("request %s: %s", l.CustomID, blockedMessage(res.Blocked)))
			return
		}
		if res.Changed && json.Unmarshal(sent, &l.Body) != nil {
			fail(http.StatusInternalServerError, codeInvalidRequest, "the redacted request could not be encoded")
			return
		}
	}

	targets, err := rt.router.Targets(provider.Batches, in.model)
	if err != nil {
		fail(http.StatusNotFound, codeModelNotFound, "model "+in.model+": "+err.Error())
		return
	}
	for i, t := range targets {
		f, ok := rt.router.Forwarder(t.Provider)
		if !ok {
			continue
		}
		body, contentType, err := fileUpload(file.Filename, in, t.Model)
		if err != nil {
			fail(http.StatusInternalServerError, codeInvalidRequest, "the file could not be encoded")
			return
		}
		resp, err := f.Forward(r.Context(), http.MethodPost, "/files", bytes.NewReader(body), contentType)
		if err != nil || (provider.Retryable(resp.StatusCode) && i < len(targets)-1) {
			if err == nil {
				_ = resp.Body.Close()
				err = fmt.Errorf("status %d", resp.StatusCode)
			}
			s.log.Warn("target failed", "request_id", requestID, "target", t.String(), "err", err)
			continue
		}
		obj, err := passThrough(w, resp, t)
		if err != nil || resp.StatusCode != http.StatusOK {
			return
		}
		var created struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(obj, &created) != nil || created.ID == "" {
			s.log.Error("file response has no id", "provider", t.Provider)
			return
		}
		if err := s.store.CreateBatchFile(context.WithoutCancel(r.Context()), &store.BatchFile{
			ID: created.ID, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, Model: in.model,
			Provider: t.Provider, UpstreamModel: t.Model, Purpose: purpose, Object: obj,
		}); err != nil {
			s.log.Error("recording file failed", "file_id", created.ID, "err", err)
		}
		s.log.Info("batch file", "request_id", requestID, "file_id", created.ID, "org_id", p.OrgID, "team_id", p.TeamID, "key_id", p.KeyID,
			"model", in.model, "target", t.String(), "requests", len(in.lines), "latency_ms", time.Since(start).Milliseconds())
		return
	}
	fail(http.StatusBadGateway, codeAllTargetsFailed, "all upstream targets failed")
}

// fileUpload encodes a batch file upload for one target.
func fileUpload(filename string, in *batchInput, upstreamModel string) ([]byte, string, error) {
	if filename == "" {
		filename = "batch.jsonl"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("purpose", "batch"); err != nil {
		return nil, "", err
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", multipart.FileContentDisposition("file", filename))
	h.Set("Content-Type", "application/jsonl")
	part, err := mw.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if err := in.writeTo(part, upstreamModel); err != nil {
		return nil, "", err
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// passThrough sends an upstream JSON response to the client and returns
// it, up to maxJobObject.
func passThrough(w http.ResponseWriter, resp *http.Response, t config.Target) ([]byte, error) {
	defer resp.Body.Close()
	obj, err := io.ReadAll(io.LimitReader(resp.Body, maxJobObject+1))
	if err != nil || len(obj) > maxJobObject {
		writeError(w, provider.Batches, http.StatusBadGateway, codeAllTargetsFailed, "reading the upstream response failed")
		return nil, errResponseTooLarge
	}
	if v := resp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	w.Header().Set("X-Gateway-Target", t.String())
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(obj)
	return obj, nil
}

// listFiles lists the caller's files, newest first, as recorded.
func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	p, ok := s.videoCaller(w, r)
	if !ok {
		return
	}
	pg, ok := clientPage(w, r)
	if !ok {
		return
	}
	files, more, err := s.store.ListBatchFiles(r.Context(), p.OrgID, p.TeamID, ownerKey(p), pg)
	if err != nil {
		s.log.Error("listing files failed", "err", err)
		writeRecordError(w, err)
		return
	}
	data := []json.RawMessage{}
	for _, f := range files {
		if p.AllowsModel(f.Model) {
			data = append(data, f.Object)
		}
	}
	writeClientList(w, data, more, len(files) > 0, func() (string, string) { return files[0].ID, files[len(files)-1].ID })
}

// fileCall serves retrieve, delete and content on a file, at the target
// that holds it.
func (s *Server) fileCall(method, suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.videoCaller(w, r)
		if !ok {
			return
		}
		f, err := s.ownedFile(r.Context(), r.PathValue("id"), p)
		if err != nil {
			writeRecordError(w, err)
			return
		}
		t := config.Target{Provider: f.Provider, Model: f.UpstreamModel}
		resp, ok := s.forward(w, r, t, method, "/files/"+url.PathEscape(f.ID)+suffix, nil, "")
		if !ok {
			return
		}
		if suffix != "" {
			// Content streams through: output files can be large.
			defer resp.Body.Close()
			for _, h := range []string{"Content-Type", "Content-Length", "Content-Disposition"} {
				if v := resp.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			w.Header().Set("X-Gateway-Target", t.String())
			w.WriteHeader(resp.StatusCode)
			copyFlush(w, resp.Body)
			return
		}
		if _, err := passThrough(w, resp, t); err == nil && method == http.MethodDelete && resp.StatusCode == http.StatusOK {
			if err := s.store.DeleteBatchFile(context.WithoutCancel(r.Context()), f.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
				s.log.Error("deleting file failed", "file_id", f.ID, "err", err)
			}
		}
	}
}

// forward sends a call to a target's provider, answering the client
// itself when it cannot.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, t config.Target, method, path string, body io.Reader, contentType string) (*http.Response, bool) {
	f, ok := s.rt.Load().router.Forwarder(t.Provider)
	if !ok {
		writeError(w, provider.Batches, http.StatusNotFound, codeModelNotFound, "the provider that holds this is no longer configured")
		return nil, false
	}
	resp, err := f.Forward(r.Context(), method, path, body, contentType)
	if err != nil {
		s.log.Warn("upstream call failed", "path", path, "provider", t.Provider, "err", err)
		writeError(w, provider.Batches, http.StatusBadGateway, codeAllTargetsFailed, "the upstream did not answer")
		return nil, false
	}
	return resp, true
}

// createBatch starts a batch on the target that holds its input file.
func (s *Server) createBatch(w http.ResponseWriter, r *http.Request) {
	p, ok := s.videoCaller(w, r)
	if !ok {
		return
	}
	fail := func(status int, code, msg string) { writeError(w, provider.Batches, status, code, msg) }
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJobObject))
	var req struct {
		InputFileID string `json:"input_file_id"`
		Endpoint    string `json:"endpoint"`
	}
	if err != nil || json.Unmarshal(raw, &req) != nil {
		fail(http.StatusBadRequest, codeInvalidRequest, "request body must be a JSON object")
		return
	}
	if !batchURLs[req.Endpoint] {
		fail(http.StatusBadRequest, codeInvalidRequest, "endpoint "+req.Endpoint+" is not served in batches")
		return
	}
	f, err := s.ownedFile(r.Context(), req.InputFileID, p)
	if err != nil || f.Purpose != "batch" {
		if err == nil {
			err = errFileNotFound
		}
		writeRecordError(w, err)
		return
	}
	requestID := r.Header.Get("X-Request-Id")
	if status, code, msg, ok := s.admit(w, r, p, requestID); !ok {
		fail(status, code, msg)
		return
	}
	t := config.Target{Provider: f.Provider, Model: f.UpstreamModel}
	resp, ok := s.forward(w, r, t, http.MethodPost, "/batches", bytes.NewReader(raw), "application/json")
	if !ok {
		return
	}
	obj, err := passThrough(w, resp, t)
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	var created struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if json.Unmarshal(obj, &created) != nil || created.ID == "" {
		s.log.Error("batch response has no id", "provider", t.Provider)
		return
	}
	b := &store.Batch{
		ID: created.ID, OrgID: p.OrgID, TeamID: p.TeamID, KeyID: p.KeyID, Model: f.Model, Provider: f.Provider,
		UpstreamModel: f.UpstreamModel, InputFileID: f.ID, Status: created.Status, Object: obj,
	}
	ctx := context.WithoutCancel(r.Context())
	if err := s.store.CreateBatch(ctx, b); err != nil {
		s.log.Error("recording batch failed", "batch_id", b.ID, "err", err)
		return
	}
	s.log.Info("batch", "request_id", requestID, "batch_id", b.ID, "org_id", p.OrgID, "team_id", p.TeamID, "key_id", p.KeyID,
		"model", b.Model, "target", t.String(), "status", b.Status)
	s.settleBatch(ctx, b, obj)
}

// batchCall serves retrieve and cancel on a batch, at its target, and
// keeps the stored batch current.
func (s *Server) batchCall(method, suffix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.videoCaller(w, r)
		if !ok {
			return
		}
		b, err := s.ownedBatch(r.Context(), r.PathValue("id"), p)
		if err != nil {
			writeRecordError(w, err)
			return
		}
		t := config.Target{Provider: b.Provider, Model: b.UpstreamModel}
		resp, ok := s.forward(w, r, t, method, "/batches/"+url.PathEscape(b.ID)+suffix, nil, "")
		if !ok {
			return
		}
		obj, err := passThrough(w, resp, t)
		if err == nil && resp.StatusCode == http.StatusOK {
			s.settleBatch(context.WithoutCancel(r.Context()), b, obj)
		}
	}
}

// listBatches lists the caller's batches, newest first, as last stored.
func (s *Server) listBatches(w http.ResponseWriter, r *http.Request) {
	p, ok := s.videoCaller(w, r)
	if !ok {
		return
	}
	pg, ok := clientPage(w, r)
	if !ok {
		return
	}
	batches, more, err := s.store.ListBatches(r.Context(), store.BatchQuery{OrgID: p.OrgID, TeamID: p.TeamID, KeyID: ownerKey(p)}, pg)
	if err != nil {
		s.log.Error("listing batches failed", "err", err)
		writeRecordError(w, err)
		return
	}
	data := []json.RawMessage{}
	for _, b := range batches {
		if p.AllowsModel(b.Model) {
			data = append(data, b.Object)
		}
	}
	writeClientList(w, data, more, len(batches) > 0, func() (string, string) { return batches[0].ID, batches[len(batches)-1].ID })
}

// clientPage reads OpenAI's limit and after.
func clientPage(w http.ResponseWriter, r *http.Request) (store.Page, bool) {
	n := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		var err error
		if n, err = strconv.Atoi(v); err != nil || n < 1 || n > 100 {
			writeError(w, provider.Batches, http.StatusBadRequest, codeInvalidRequest, "limit must be between 1 and 100")
			return store.Page{}, false
		}
	}
	return store.Page{Limit: n, After: r.URL.Query().Get("after")}, true
}

func writeClientList(w http.ResponseWriter, data []json.RawMessage, more, nonEmpty bool, ids func() (string, string)) {
	out := map[string]any{"object": "list", "data": data, "has_more": more}
	if nonEmpty {
		out["first_id"], out["last_id"] = ids()
	}
	writeJSON(w, http.StatusOK, out)
}

// batchObject is what the gateway reads from a batch object.
type batchObject struct {
	Status       string          `json:"status"`
	OutputFileID string          `json:"output_file_id"`
	ErrorFileID  string          `json:"error_file_id"`
	Usage        json.RawMessage `json:"usage"`
}

// settleBatch stores a batch object the upstream returned, records the
// output files it names for the batch's owner, and charges the batch
// the first time it is seen finished.
func (s *Server) settleBatch(ctx context.Context, b *store.Batch, obj []byte) {
	var o batchObject
	if json.Unmarshal(obj, &o) != nil || o.Status == "" {
		return
	}
	if err := s.store.UpdateBatch(ctx, b.ID, o.Status, obj); err != nil {
		s.log.Error("updating batch failed", "batch_id", b.ID, "err", err)
		return
	}
	for _, id := range []string{o.OutputFileID, o.ErrorFileID} {
		if id == "" {
			continue
		}
		fobj, _ := json.Marshal(map[string]any{"id": id, "object": "file", "purpose": "batch_output", "created_at": time.Now().Unix()})
		if err := s.store.CreateBatchFile(ctx, &store.BatchFile{
			ID: id, OrgID: b.OrgID, TeamID: b.TeamID, KeyID: b.KeyID, Model: b.Model,
			Provider: b.Provider, UpstreamModel: b.UpstreamModel, Purpose: "batch_output", Object: fobj,
		}); err != nil {
			s.log.Error("recording batch output file failed", "batch_id", b.ID, "file_id", id, "err", err)
		}
	}
	if !store.BatchDone(o.Status) || b.ChargedAt != nil {
		return
	}
	var tokens usage.Tokens
	if len(o.Usage) > 0 && string(o.Usage) != "null" {
		tokens, _ = usage.FromOpenAI(obj)
	}
	price := s.targetPrice(b.Model, b.Provider, b.UpstreamModel)
	cost := usage.Cost(tokens, price) * (1 - price.BatchDiscount)
	claimed, err := s.store.ClaimBatchCharge(ctx, b.ID, cost)
	if err != nil || !claimed {
		if err != nil {
			s.log.Error("charging batch failed", "batch_id", b.ID, "err", err)
		}
		return
	}
	p := s.batchPrincipal(ctx, b)
	spent, err := s.limiter.Charge(ctx, p, tokens.Total(), cost)
	if err != nil {
		s.log.Error("charging limits failed", "batch_id", b.ID, "err", err)
	}
	s.budgetAlerts(ctx, p, spent)
	if s.usage != nil {
		s.usage.Add(usage.Record{
			RequestID: b.ID, Time: time.Now(), OrgID: b.OrgID, TeamID: b.TeamID, KeyID: b.KeyID,
			Endpoint: string(provider.Batches), Model: b.Model, Provider: b.Provider, UpstreamModel: b.UpstreamModel,
			Status: http.StatusOK, Tokens: tokens, CostUSD: cost,
		})
	}
	model := b.Model
	if !s.rt.Load().router.Has(model) {
		model = ""
	}
	s.metrics.ObserveRequest(telemetry.Request{
		Endpoint: string(provider.Batches), Model: model, Target: b.Provider + "/" + b.UpstreamModel, Status: http.StatusOK,
		Tokens: tokens, CostUSD: cost, Unpriced: price.IsZero() && tokens.Total() > 0,
	})
	s.log.Info("batch charged", "batch_id", b.ID, "org_id", b.OrgID, "team_id", b.TeamID, "status", o.Status,
		"input_tokens", tokens.InputTokens, "output_tokens", tokens.OutputTokens, "cost_usd", cost)
}

// targetPrice is the configured price of a model's target, or zero when
// it is no longer configured.
func (s *Server) targetPrice(model, providerName, upstreamModel string) config.Price {
	for _, t := range s.rt.Load().cfg.Models[model].Targets {
		if t.Provider == providerName && t.Model == upstreamModel {
			return t.Price
		}
	}
	return config.Price{}
}

// batchPrincipal is the caller a batch is charged to: its key when the
// key still exists, otherwise its team's and organization's policies.
func (s *Server) batchPrincipal(ctx context.Context, b *store.Batch) *auth.Principal {
	if p, ok := s.keys.Load().ByKeyID(b.KeyID); ok {
		return p
	}
	p := &auth.Principal{OrgID: b.OrgID, TeamID: b.TeamID, KeyID: b.KeyID}
	team, err := s.store.GetTeam(ctx, b.TeamID)
	if err != nil {
		return p
	}
	if org, err := s.store.GetOrg(ctx, b.OrgID); err == nil {
		p.Policies = [3]policy.Policy{org.Policy, team.Policy, {}}
	}
	return p
}

// pollBatches refreshes unfinished batches that no client has looked at
// lately, so they are charged and shown current without polling.
func (s *Server) pollBatches(ctx context.Context) {
	due, err := s.store.ClaimBatchesToPoll(ctx, time.Now().Add(-batchStale), batchPollMax)
	if err != nil {
		s.log.Error("listing batches to refresh failed", "err", err)
	}
	for _, b := range due {
		obj, err := s.fetchBatch(ctx, b, "")
		if err != nil {
			s.log.Warn("refreshing batch failed", "batch_id", b.ID, "provider", b.Provider, "err", err)
			continue
		}
		s.settleBatch(ctx, b, obj)
	}
}

// fetchBatch calls a batch at its target, for the refresh loop and the
// admin API, and returns the batch object.
func (s *Server) fetchBatch(ctx context.Context, b *store.Batch, suffix string) ([]byte, error) {
	f, ok := s.rt.Load().router.Forwarder(b.Provider)
	if !ok {
		return nil, errors.New("provider is no longer configured")
	}
	method := http.MethodGet
	if suffix != "" {
		method = http.MethodPost
	}
	resp, err := f.Forward(ctx, method, "/batches/"+url.PathEscape(b.ID)+suffix, nil, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	obj, err := io.ReadAll(io.LimitReader(resp.Body, maxJobObject+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || len(obj) > maxJobObject {
		return nil, fmt.Errorf("upstream answered %d", resp.StatusCode)
	}
	return obj, nil
}

// batchPollLoop runs pollBatches every minute.
func (s *Server) batchPollLoop(ctx context.Context) {
	t := time.NewTicker(batchPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.pollBatches(ctx)
		}
	}
}

// --- admin API ---

// listAdminBatches lists batches for operators, newest first.
func (s *Server) listAdminBatches(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := s.readScope(r, q.Get("org_id"), q.Get("team_id"), ""); err != nil {
		return err
	}
	items, more, err := s.store.ListBatches(r.Context(), store.BatchQuery{OrgID: q.Get("org_id"), TeamID: q.Get("team_id"), Status: q.Get("status")}, pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}

// cancelAdminBatch cancels a batch at its target, for operators who can
// write to its organization. It is audited.
func (s *Server) cancelAdminBatch(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	b, err := s.store.GetBatch(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !operatorFrom(ctx).canRead(b.OrgID) {
		return store.ErrNotFound
	}
	if err := write(r, b.OrgID); err != nil {
		return err
	}
	obj, err := s.fetchBatch(ctx, b, "/cancel")
	if err != nil {
		return &apiError{http.StatusBadGateway, "upstream_error", "the provider did not cancel the batch: " + err.Error()}
	}
	s.settleBatch(context.WithoutCancel(ctx), b, obj)
	if err := s.audit(ctx, "batch.cancel", "batch", b.ID, b.OrgID, map[string]any{"team_id": b.TeamID, "model": b.Model}); err != nil {
		return err
	}
	if b, err = s.store.GetBatch(ctx, b.ID); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, b)
	return nil
}

// listAdminVideos lists video jobs for operators, newest first.
func (s *Server) listAdminVideos(w http.ResponseWriter, r *http.Request) error {
	pg, err := page(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	if err := s.readScope(r, q.Get("org_id"), q.Get("team_id"), ""); err != nil {
		return err
	}
	items, more, err := s.store.ListVideoJobs(r.Context(), q.Get("org_id"), q.Get("team_id"), "", pg)
	if err != nil {
		return err
	}
	return writeList(w, items, more)
}
