package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// snapshotSource is where a replica reads tenancy and stored
// configuration from: its store, or its control plane (ADR 0034).
type snapshotSource interface {
	ConfigRevision(ctx context.Context) (int64, error)
	ListConfigObjects(ctx context.Context) ([]*store.ConfigObject, int64, error)
	LoadSnapshot(ctx context.Context) (*store.Snapshot, error)
	ListTeamCredentials(ctx context.Context, teamID string) ([]*store.TeamCredential, error)
	LoadCedarPolicies(ctx context.Context) (map[string]*store.CedarPolicies, error)
}

// planeSnapshot is what a control plane sends its data planes: every
// stored input of a replica's runtime, with the secrets the admin API
// never returns. Secrets stay sealed with the encryption key, which
// both planes share.
type planeSnapshot struct {
	Version     string                          `json:"version"`
	Revision    int64                           `json:"config_revision"`
	Objects     []planeObject                   `json:"config_objects"`
	Orgs        []*store.Org                    `json:"orgs"`
	Teams       []*store.Team                   `json:"teams"`
	Keys        []planeKey                      `json:"keys"`
	Credentials []planeCredential               `json:"team_credentials"`
	Cedar       map[string]*store.CedarPolicies `json:"cedar"`
}

type planeObject struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Spec    string `json:"spec"`
	Secrets string `json:"secrets,omitempty"`
}

type planeKey struct {
	*store.Key
	SecretHash   string `json:"secret_hash"`
	PreviousHash string `json:"previous_hash,omitempty"`
}

type planeCredential struct {
	*store.TeamCredential
	Secret string `json:"secret"`
}

// Paths of the control plane's endpoints for data planes.
const (
	planeSnapshotPath = "/cp/v1/snapshot"
	planeRecordsPath  = "/cp/v1/records/"
)

// maxPlaneBody bounds a batch of records a data plane sends.
const maxPlaneBody = 64 << 20

// planeAuthorized checks a data plane's token.
func (s *Server) planeAuthorized(w http.ResponseWriter, r *http.Request) bool {
	want := s.rt.Load().cfg.DataPlanes.Token.Value()
	got := presentedKey(r)
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		writeError(w, "", http.StatusUnauthorized, codeInvalidKey, "missing or invalid data plane token")
		return false
	}
	return true
}

// buildSnapshot reads a snapshot from the store.
func buildSnapshot(ctx context.Context, st *store.Store) (*planeSnapshot, error) {
	rows, rev, err := st.ListConfigObjects(ctx)
	if err != nil {
		return nil, err
	}
	snap, err := st.LoadSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	creds, err := st.ListTeamCredentials(ctx, "")
	if err != nil {
		return nil, err
	}
	cedar, err := st.LoadCedarPolicies(ctx)
	if err != nil {
		return nil, err
	}
	out := &planeSnapshot{Revision: rev, Cedar: cedar, Objects: []planeObject{}, Keys: []planeKey{}, Credentials: []planeCredential{}}
	for _, o := range rows {
		out.Objects = append(out.Objects, planeObject{Kind: o.Kind, Name: o.Name, Spec: o.Spec, Secrets: o.Secrets})
	}
	for _, o := range snap.Orgs {
		out.Orgs = append(out.Orgs, o)
	}
	for _, t := range snap.Teams {
		out.Teams = append(out.Teams, t)
	}
	slices.SortFunc(out.Orgs, func(a, b *store.Org) int { return compareID(a.ID, b.ID) })
	slices.SortFunc(out.Teams, func(a, b *store.Team) int { return compareID(a.ID, b.ID) })
	for _, k := range snap.Keys {
		out.Keys = append(out.Keys, planeKey{Key: k, SecretHash: k.SecretHash, PreviousHash: k.PreviousHash})
	}
	for _, c := range creds {
		out.Credentials = append(out.Credentials, planeCredential{TeamCredential: c, Secret: c.Secret})
	}
	// The version names the content, so an unchanged snapshot is not
	// sent again.
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	out.Version = hex.EncodeToString(sum[:16])
	return out, nil
}

func compareID(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// serveSnapshot sends the current snapshot, or 304 when the data plane
// already has this version.
func (s *Server) serveSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.planeAuthorized(w, r) {
		return
	}
	snap, err := buildSnapshot(r.Context(), s.store)
	if err != nil {
		s.log.Error("building a data plane snapshot failed", "err", err)
		writeError(w, "", http.StatusServiceUnavailable, codeInvalidRequest, "the snapshot could not be read")
		return
	}
	etag := `"` + snap.Version + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

// serveRecords stores a batch of records a data plane wrote: usage, MCP
// calls or guardrail events. They are published to streams as if this
// replica had written them.
func (s *Server) serveRecords(w http.ResponseWriter, r *http.Request) {
	if !s.planeAuthorized(w, r) {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPlaneBody))
	if err != nil {
		writeError(w, "", http.StatusRequestEntityTooLarge, codeTooLarge, "the batch is too large")
		return
	}
	ctx := r.Context()
	switch r.PathValue("kind") {
	case config.RecordUsage:
		err = insertRecords(ctx, body, streamed(s, config.RecordUsage, s.store.InsertUsage))
	case config.RecordMCPCalls:
		err = insertRecords(ctx, body, streamed(s, config.RecordMCPCalls, s.store.InsertMCPCalls))
	case config.RecordGuardrailEvents:
		err = insertRecords(ctx, body, streamed(s, config.RecordGuardrailEvents, s.store.InsertGuardrailEvents))
	case config.RecordDenials:
		err = insertRecords(ctx, body, streamed(s, config.RecordDenials, s.store.InsertDenials))
	default:
		writeError(w, "", http.StatusNotFound, codeNotFound, "unknown record kind")
		return
	}
	var bad *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &bad) || errors.As(err, &typ):
		writeError(w, "", http.StatusBadRequest, codeInvalidRequest, "the batch is not a JSON list of records")
	case err != nil:
		s.log.Error("storing data plane records failed", "kind", r.PathValue("kind"), "err", err)
		writeError(w, "", http.StatusServiceUnavailable, codeInvalidRequest, "the records could not be stored")
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func insertRecords[T any](ctx context.Context, body []byte, write func(context.Context, []T) error) error {
	var recs []T
	if err := json.Unmarshal(body, &recs); err != nil {
		return err
	}
	if len(recs) == 0 {
		return nil
	}
	return write(ctx, recs)
}

// planeClient is a data plane's view of its control plane. It holds the
// last snapshot and serves it as a snapshotSource.
type planeClient struct {
	cfg    config.ControlPlane
	client *http.Client
	log    interface {
		Warn(string, ...any)
	}

	mu   sync.Mutex
	snap *planeSnapshot
	// store is the snapshot in the store's types.
	store *store.Snapshot
}

func newPlaneClient(cfg config.ControlPlane, client *http.Client, log interface{ Warn(string, ...any) }) *planeClient {
	return &planeClient{cfg: cfg, client: client, log: log}
}

// update fetches the snapshot when it changed. When the control plane
// cannot be reached before any snapshot is held, the cached one is
// loaded, and update reports the error after loading it.
func (c *planeClient) update(ctx context.Context) error {
	err := c.fetch(ctx)
	if err == nil {
		return nil
	}
	c.mu.Lock()
	have := c.snap != nil
	c.mu.Unlock()
	if have || c.cfg.CacheFile == "" {
		return err
	}
	raw, rerr := os.ReadFile(c.cfg.CacheFile)
	if rerr != nil {
		return errors.Join(err, fmt.Errorf("no cached snapshot: %w", rerr))
	}
	var snap planeSnapshot
	if jerr := json.Unmarshal(raw, &snap); jerr != nil {
		return errors.Join(err, fmt.Errorf("the cached snapshot does not parse: %w", jerr))
	}
	c.install(&snap)
	c.log.Warn("the control plane cannot be reached; serving the cached snapshot", "version", snap.Version, "err", err)
	return nil
}

func (c *planeClient) fetch(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.URL+planeSnapshotPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token.Value())
	c.mu.Lock()
	if c.snap != nil {
		req.Header.Set("If-None-Match", `"`+c.snap.Version+`"`)
	}
	c.mu.Unlock()
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotModified:
		return nil
	case http.StatusOK:
	default:
		return fmt.Errorf("the control plane answered HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<30))
	if err != nil {
		return err
	}
	var snap planeSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("the snapshot does not parse: %w", err)
	}
	c.install(&snap)
	if c.cfg.CacheFile != "" {
		if err := writeFileAtomic(c.cfg.CacheFile, raw); err != nil {
			c.log.Warn("caching the snapshot failed", "file", c.cfg.CacheFile, "err", err)
		}
	}
	return nil
}

// install makes a snapshot current.
func (c *planeClient) install(snap *planeSnapshot) {
	st := &store.Snapshot{Orgs: map[string]*store.Org{}, Teams: map[string]*store.Team{}}
	for _, o := range snap.Orgs {
		st.Orgs[o.ID] = o
	}
	for _, t := range snap.Teams {
		st.Teams[t.ID] = t
	}
	for _, k := range snap.Keys {
		if k.Key == nil {
			continue
		}
		key := *k.Key
		key.SecretHash, key.PreviousHash = k.SecretHash, k.PreviousHash
		st.Keys = append(st.Keys, &key)
	}
	c.mu.Lock()
	c.snap, c.store = snap, st
	c.mu.Unlock()
}

func (c *planeClient) current() (*planeSnapshot, *store.Snapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap == nil {
		return nil, nil, errors.New("no snapshot from the control plane yet")
	}
	return c.snap, c.store, nil
}

// ConfigRevision implements snapshotSource.
func (c *planeClient) ConfigRevision(context.Context) (int64, error) {
	snap, _, err := c.current()
	if err != nil {
		return 0, err
	}
	return snap.Revision, nil
}

// ListConfigObjects implements snapshotSource. Settings stay with each
// data plane's own file: a data plane has no store, which some
// settings need.
func (c *planeClient) ListConfigObjects(context.Context) ([]*store.ConfigObject, int64, error) {
	snap, _, err := c.current()
	if err != nil {
		return nil, 0, err
	}
	var out []*store.ConfigObject
	for _, o := range snap.Objects {
		if o.Kind == config.KindSettings {
			continue
		}
		out = append(out, &store.ConfigObject{Kind: o.Kind, Name: o.Name, Spec: o.Spec, Secrets: o.Secrets})
	}
	return out, snap.Revision, nil
}

// LoadSnapshot implements snapshotSource.
func (c *planeClient) LoadSnapshot(context.Context) (*store.Snapshot, error) {
	_, st, err := c.current()
	return st, err
}

// ListTeamCredentials implements snapshotSource.
func (c *planeClient) ListTeamCredentials(_ context.Context, teamID string) ([]*store.TeamCredential, error) {
	snap, _, err := c.current()
	if err != nil {
		return nil, err
	}
	var out []*store.TeamCredential
	for _, pc := range snap.Credentials {
		if pc.TeamCredential == nil || (teamID != "" && pc.TeamID != teamID) {
			continue
		}
		tc := *pc.TeamCredential
		tc.Secret = pc.Secret
		out = append(out, &tc)
	}
	return out, nil
}

// LoadCedarPolicies implements snapshotSource.
func (c *planeClient) LoadCedarPolicies(context.Context) (map[string]*store.CedarPolicies, error) {
	snap, _, err := c.current()
	if err != nil {
		return nil, err
	}
	if snap.Cedar == nil {
		return map[string]*store.CedarPolicies{}, nil
	}
	return snap.Cedar, nil
}

// send returns a sink that posts a batch of records to the control
// plane. A failed batch is retried by the writer.
func planeSink[T any](c *planeClient, kind string) func(context.Context, []T) error {
	return func(ctx context.Context, recs []T) error {
		body, err := json.Marshal(recs)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+planeRecordsPath+kind, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.cfg.Token.Value())
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			return fmt.Errorf("the control plane answered HTTP %d", resp.StatusCode)
		}
		return nil
	}
}

// writeFileAtomic replaces a file, readable only by its owner, so a
// crash never leaves half a snapshot.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// newPlaneWriters makes the record writers of a data plane, which send
// to the control plane.
func newPlaneWriters(s *Server) {
	c, log := s.plane, s.log
	s.usage = usage.NewWriter(planeSink[usage.Record](c, config.RecordUsage), log, "usage")
	s.calls = usage.NewWriter(planeSink[store.MCPCall](c, config.RecordMCPCalls), log, "mcp_calls")
	s.guardEvents = usage.NewWriter(planeSink[store.GuardrailEvent](c, config.RecordGuardrailEvents), log, "guardrail_events")
	s.denials = usage.NewWriter(planeSink[store.Denial](c, config.RecordDenials), log, "denials")
}
