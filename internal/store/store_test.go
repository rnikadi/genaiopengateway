package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/policy"
	"github.com/rnikadi/genaiopengateway/internal/usage"
)

// backends returns a fresh store per available database. Postgres runs
// when GOG_TEST_POSTGRES_DSN points at an empty database.
func backends(t *testing.T) map[string]*Store {
	t.Helper()
	ctx := context.Background()
	out := map[string]*Store{}
	s, err := Open(ctx, "sqlite", "file:"+filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	out["sqlite"] = s

	if dsn := os.Getenv("GOG_TEST_POSTGRES_DSN"); dsn != "" {
		pg, err := Open(ctx, "postgres", dsn)
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"usage", "audit_log", "mcp_calls", "mcp_tool_pins", "api_keys", "teams", "orgs"} {
			if _, err := pg.db.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { pg.Close() })
		out["postgres"] = pg
	}
	return out
}

func TestTenancyLifecycle(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			org := &Org{Name: "acme", Policy: policy.Policy{RPM: 100, Budget: &policy.Budget{AmountUSD: 50, Period: policy.PeriodMonthly}}}
			if err := s.CreateOrg(ctx, org); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetOrg(ctx, org.ID)
			if err != nil || got.Name != "acme" || got.Policy.RPM != 100 || got.Policy.Budget.AmountUSD != 50 {
				t.Fatalf("GetOrg = %+v, %v", got, err)
			}

			team := &Team{OrgID: org.ID, Name: "search"}
			if err := s.CreateTeam(ctx, team); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateTeam(ctx, &Team{OrgID: "org_missing", Name: "x"}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("team in missing org: %v", err)
			}

			exp := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
			key := &Key{TeamID: team.ID, Name: "ci", Prefix: "sk-gog-abcd", SecretHash: "hash1", ExpiresAt: &exp}
			if err := s.CreateKey(ctx, key); err != nil {
				t.Fatal(err)
			}
			if key.OrgID != org.ID {
				t.Fatalf("key org = %s", key.OrgID)
			}
			if err := s.CreateKey(ctx, &Key{TeamID: team.ID, Name: "dup", SecretHash: "hash1"}); err == nil {
				t.Fatal("duplicate hash accepted")
			}

			snap, err := s.LoadSnapshot(ctx)
			if err != nil || len(snap.Keys) != 1 || snap.Orgs[org.ID] == nil || snap.Teams[team.ID] == nil {
				t.Fatalf("snapshot = %+v, %v", snap, err)
			}
			if !snap.Keys[0].ExpiresAt.Equal(exp) {
				t.Fatalf("expiry = %v, want %v", snap.Keys[0].ExpiresAt, exp)
			}

			if err := s.DeleteOrg(ctx, org.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("delete org with teams: %v", err)
			}
			if err := s.DeleteTeam(ctx, team.ID); !errors.Is(err, ErrConflict) {
				t.Fatalf("delete team with keys: %v", err)
			}
			if err := s.RevokeKey(ctx, key.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.RevokeKey(ctx, key.ID); err != nil {
				t.Fatalf("second revoke: %v", err)
			}
			if snap, _ := s.LoadSnapshot(ctx); len(snap.Keys) != 0 {
				t.Fatal("revoked key in snapshot")
			}
			if err := s.DeleteTeam(ctx, team.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteOrg(ctx, org.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetOrg(ctx, org.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted org: %v", err)
			}
			if err := s.UpdateOrg(ctx, org); !errors.Is(err, ErrNotFound) {
				t.Fatalf("update deleted org: %v", err)
			}
		})
	}
}

func TestPagination(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for i := 0; i < 5; i++ {
				if err := s.CreateOrg(ctx, &Org{Name: "o"}); err != nil {
					t.Fatal(err)
				}
			}
			page1, more, err := s.ListOrgs(ctx, Page{Limit: 3})
			if err != nil || len(page1) != 3 || !more {
				t.Fatalf("page1 = %d %v %v", len(page1), more, err)
			}
			page2, more, err := s.ListOrgs(ctx, Page{Limit: 3, After: page1[2].ID})
			if err != nil || len(page2) != 2 || more {
				t.Fatalf("page2 = %d %v %v", len(page2), more, err)
			}
		})
	}
}

func TestUsageAggregationAndSpend(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			org := &Org{Name: "o"}
			_ = s.CreateOrg(ctx, org)
			team := &Team{OrgID: org.ID, Name: "t"}
			_ = s.CreateTeam(ctx, team)
			key := &Key{TeamID: team.ID, Name: "k", SecretHash: "h"}
			_ = s.CreateKey(ctx, key)

			day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			rec := func(model string, at time.Time, cost float64) usage.Record {
				return usage.Record{RequestID: "r", Time: at, OrgID: org.ID, TeamID: team.ID, KeyID: key.ID, Endpoint: "chat.completions",
					Model: model, Provider: "p", UpstreamModel: "u", Status: 200, Tokens: usage.Tokens{InputTokens: 10, OutputTokens: 5}, CostUSD: cost}
			}
			err := s.InsertUsage(ctx, []usage.Record{rec("chat", day, 1.5), rec("chat", day.Add(time.Hour), 0.5), rec("embed", day.Add(24*time.Hour), 2)})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := s.QueryUsage(ctx, UsageQuery{OrgID: org.ID, From: day.Add(-time.Hour), To: day.Add(48 * time.Hour), GroupBy: "model"})
			if err != nil || len(rows) != 2 || rows[0].Group != "chat" || rows[0].Requests != 2 || rows[0].CostUSD != 2 || rows[0].InputTokens != 20 {
				t.Fatalf("by model = %+v, %v", rows, err)
			}
			rows, err = s.QueryUsage(ctx, UsageQuery{From: day.Add(-time.Hour), To: day.Add(48 * time.Hour), GroupBy: "day"})
			if err != nil || len(rows) != 2 || rows[0].Group != "2026-10-01" || rows[1].Group != "2026-10-02" {
				t.Fatalf("by day = %+v, %v", rows, err)
			}
			if _, err := s.QueryUsage(ctx, UsageQuery{GroupBy: "bogus"}); err == nil {
				t.Fatal("bad group_by accepted")
			}

			spend, err := s.SpendSince(ctx, day.Add(30*time.Minute))
			if err != nil || spend[org.ID] != 2.5 || spend[team.ID] != 2.5 || spend[key.ID] != 2.5 {
				t.Fatalf("spend = %v, %v", spend, err)
			}
			k, _ := s.GetKey(ctx, key.ID)
			if k.LastUsedAt == nil || !k.LastUsedAt.Equal(day.Add(24*time.Hour)) {
				t.Fatalf("last_used_at = %v", k.LastUsedAt)
			}
			n, err := s.PruneUsage(ctx, day.Add(2*time.Hour))
			if err != nil || n != 2 {
				t.Fatalf("pruned %d, %v", n, err)
			}
		})
	}
}

func TestAudit(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, action := range []string{"org.create", "team.create", "key.create"} {
				if err := s.AppendAudit(ctx, &AuditEntry{Actor: "admin", Action: action, ResourceType: "x", ResourceID: "1", OrgID: "org_a", Details: json.RawMessage(`{"a":1}`)}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(2 * time.Millisecond)
			}
			_ = s.AppendAudit(ctx, &AuditEntry{Actor: "admin", Action: "org.create", OrgID: "org_b"})
			entries, more, err := s.ListAudit(ctx, "org_a", Page{Limit: 2})
			if err != nil || len(entries) != 2 || !more || entries[0].Action != "key.create" {
				t.Fatalf("audit = %+v %v %v", entries, more, err)
			}
			rest, more, _ := s.ListAudit(ctx, "org_a", Page{Limit: 2, After: entries[1].ID})
			if len(rest) != 1 || more || rest[0].Action != "org.create" {
				t.Fatalf("audit page 2 = %+v", rest)
			}
			all, _, _ := s.ListAudit(ctx, "", Page{})
			if len(all) != 4 {
				t.Fatalf("all audit = %d", len(all))
			}
		})
	}
}

func TestMigrationsAreIdempotent(t *testing.T) {
	path := "file:" + filepath.Join(t.TempDir(), "m.db")
	for i := 0; i < 2; i++ {
		s, err := Open(context.Background(), "sqlite", path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		s.Close()
	}
}
