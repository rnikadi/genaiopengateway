package store

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func openSQLite(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), "sqlite", "file:"+filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedForBackup(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	org := &Org{Name: "acme", Policy: policy.Policy{RPM: 100, Budget: &policy.Budget{AmountUSD: 50.5, Period: policy.PeriodMonthly}}}
	if err := s.CreateOrg(ctx, org); err != nil {
		t.Fatal(err)
	}
	team := &Team{OrgID: org.ID, Name: "search"}
	if err := s.CreateTeam(ctx, team); err != nil {
		t.Fatal(err)
	}
	exp := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
	key := &Key{TeamID: team.ID, Name: "ci", Prefix: "sk-gog-abcd", SecretHash: "hash-backup", ExpiresAt: &exp, Owner: "ci@example.com"}
	if err := s.CreateKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	recs := []usage.Record{
		{RequestID: "req_1", Time: time.Now().UTC(), OrgID: org.ID, TeamID: team.ID, KeyID: key.ID, Endpoint: "chat", Model: "m", Provider: "p", Status: 200, LatencyMS: 12, CostUSD: 0.0123, Tokens: usage.Tokens{InputTokens: 10, OutputTokens: 5}},
		{RequestID: "req_2", Time: time.Now().UTC(), OrgID: org.ID, TeamID: team.ID, KeyID: key.ID, Endpoint: "audio.speech", Model: "tts", Provider: "p", Status: 200, CostUSD: 1, CacheHit: true, SavedUSD: 0.5},
	}
	if err := s.InsertUsage(ctx, recs); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutConfigObject(ctx, &ConfigObject{Kind: "provider", Name: "openai", Spec: `{"type":"openai"}`, Secrets: "enc:abc", UpdatedBy: "test"}); err != nil {
		t.Fatal(err)
	}
}

func keyID(t *testing.T, s *Store) string {
	t.Helper()
	var id string
	if err := s.db.QueryRow(`SELECT id FROM api_keys`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// archiveRows reads an archive into table → its rows as sorted JSON.
func archiveRows(t *testing.T, b []byte) (BackupHeader, map[string][]string) {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(gz)
	var h BackupHeader
	if err := dec.Decode(&h); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for {
		var r struct {
			T string          `json:"t"`
			R json.RawMessage `json:"r"`
		}
		if err := dec.Decode(&r); err != nil {
			break
		}
		out[r.T] = append(out[r.T], string(r.R))
	}
	for _, rows := range out {
		slices.Sort(rows)
	}
	return h, out
}

func backupBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := s.Backup(context.Background(), &buf, BackupOptions{KeyCheck: KeyCheck("k")}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestBackupRestoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t)
	seedForBackup(t, src)
	archive := backupBytes(t, src)
	h, want := archiveRows(t, archive)
	if h.Format != BackupFormat || h.Schema != latestMigration() || h.Driver != "sqlite" || h.KeyCheck != KeyCheck("k") || len(h.KeyCheck) != 12 {
		t.Fatalf("header = %+v", h)
	}
	if h.Tables["orgs"] != 1 || h.Tables["usage"] != 2 || h.Tables["config_objects"] != 1 {
		t.Fatalf("counts = %v", h.Tables)
	}
	if _, ok := h.Tables["admin_sessions"]; ok {
		t.Fatal("sessions are backed up")
	}

	targets := map[string]func() *Store{"sqlite": func() *Store { return openSQLite(t) }}
	if dsn := os.Getenv("GOG_TEST_POSTGRES_DSN"); dsn != "" {
		targets["postgres"] = func() *Store {
			pg, err := Open(ctx, "postgres", dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { pg.Close() })
			return pg
		}
	}
	for name, open := range targets {
		t.Run(name, func(t *testing.T) {
			dst := open()
			rev0, _ := dst.ConfigRevision(ctx)
			// The shared Postgres test database holds other tests' rows.
			if _, err := dst.Restore(ctx, bytes.NewReader(archive), RestoreOptions{Replace: name == "postgres"}); err != nil {
				t.Fatal(err)
			}
			_, got := archiveRows(t, backupBytes(t, dst))
			for table, rows := range want {
				if !slices.Equal(got[table], rows) {
					t.Errorf("%s after restore:\n got %v\nwant %v", table, got[table], rows)
				}
			}
			if rev, _ := dst.ConfigRevision(ctx); rev == rev0 {
				t.Error("restore did not move the config revision, so replicas would not reload")
			}
			if k, err := dst.GetKey(ctx, keyID(t, src)); err != nil || k.Name != "ci" || k.SecretHash != "hash-backup" {
				t.Fatalf("key after restore = %+v, %v", k, err)
			}

			// Postgres back to SQLite gives the same rows too.
			if name == "postgres" {
				again := openSQLite(t)
				if _, err := again.Restore(ctx, bytes.NewReader(backupBytes(t, dst)), RestoreOptions{}); err != nil {
					t.Fatal(err)
				}
				_, back := archiveRows(t, backupBytes(t, again))
				for table, rows := range want {
					if !slices.Equal(back[table], rows) {
						t.Errorf("%s after postgres to sqlite:\n got %v\nwant %v", table, back[table], rows)
					}
				}
			}
		})
	}
}

func TestRestoreRefusesNonEmptyUnlessReplace(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t)
	seedForBackup(t, src)
	archive := backupBytes(t, src)

	dst := openSQLite(t)
	if err := dst.CreateOrg(ctx, &Org{Name: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Restore(ctx, bytes.NewReader(archive), RestoreOptions{}); !errors.Is(err, ErrNotEmpty) {
		t.Fatalf("restore into a used database: %v", err)
	}
	if _, err := dst.Restore(ctx, bytes.NewReader(archive), RestoreOptions{Replace: true}); err != nil {
		t.Fatal(err)
	}
	orgs, _, err := dst.ListOrgs(ctx, Page{})
	if err != nil || len(orgs) != 1 || orgs[0].Name != "acme" {
		t.Fatalf("orgs after replace = %+v, %v", orgs, err)
	}
}

func TestRestoreIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	src := openSQLite(t)
	seedForBackup(t, src)
	archive := backupBytes(t, src)

	// Cut the archive short: the counts in the header no longer match.
	gz, _ := gzip.NewReader(bytes.NewReader(archive))
	var plain bytes.Buffer
	_, _ = plain.ReadFrom(gz)
	lines := strings.Split(strings.TrimSpace(plain.String()), "\n")
	var cut bytes.Buffer
	w := gzip.NewWriter(&cut)
	_, _ = w.Write([]byte(strings.Join(lines[:len(lines)-1], "\n") + "\n"))
	_ = w.Close()

	dst := openSQLite(t)
	if _, err := dst.Restore(ctx, &cut, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("truncated archive: %v", err)
	}
	if orgs, _, _ := dst.ListOrgs(ctx, Page{}); len(orgs) != 0 {
		t.Fatalf("a failed restore left %d organizations", len(orgs))
	}
}

func TestRestoreRefusesNewerSchema(t *testing.T) {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_ = json.NewEncoder(w).Encode(BackupHeader{Format: BackupFormat, Schema: "9999_future", Tables: map[string]int64{}})
	_ = w.Close()
	_, err := openSQLite(t).Restore(context.Background(), &buf, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "newer gateway") {
		t.Fatalf("newer schema: %v", err)
	}
	if _, err := openSQLite(t).Restore(context.Background(), strings.NewReader("not gzip"), RestoreOptions{}); err == nil {
		t.Fatal("garbage accepted")
	}
}
