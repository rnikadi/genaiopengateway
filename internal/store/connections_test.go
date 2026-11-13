package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMCPConnections(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			exp := time.Now().Add(time.Hour).UTC().Truncate(time.Millisecond)
			for _, sub := range []string{"bob", "alice"} {
				if err := s.PutMCPConnection(ctx, &MCPConnection{Server: "gh", Subject: sub, AccessToken: "a", RefreshToken: "r", Scopes: []string{"repo"}, ExpiresAt: &exp}); err != nil {
					t.Fatal(err)
				}
			}
			c, err := s.GetMCPConnection(ctx, "gh", "alice")
			if err != nil || c.AccessToken != "a" || !c.ExpiresAt.Equal(exp) || c.Scopes[0] != "repo" {
				t.Fatalf("get = %+v, %v", c, err)
			}
			// Replacing tokens keeps the creation time.
			time.Sleep(5 * time.Millisecond)
			if err := s.PutMCPConnection(ctx, &MCPConnection{Server: "gh", Subject: "alice", AccessToken: "a2"}); err != nil {
				t.Fatal(err)
			}
			c2, _ := s.GetMCPConnection(ctx, "gh", "alice")
			if c2.AccessToken != "a2" || !c2.CreatedAt.Equal(c.CreatedAt) || c2.ExpiresAt != nil || len(c2.Scopes) != 0 {
				t.Fatalf("replaced = %+v", c2)
			}
			if err := s.TouchMCPConnection(ctx, "gh", "bob", time.Now().Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			if sub, err := s.RecentMCPSubject(ctx, "gh"); err != nil || sub != "bob" {
				t.Fatalf("recent = %q, %v", sub, err)
			}
			if sub, _ := s.RecentMCPSubject(ctx, "linear"); sub != "" {
				t.Fatalf("recent for an unconnected server = %q", sub)
			}
			list, more, err := s.ListMCPConnections(ctx, "gh", "", Page{Limit: 1})
			if err != nil || !more || list[0].Subject != "alice" {
				t.Fatalf("page 1 = %+v, %v", list, err)
			}
			list, more, _ = s.ListMCPConnections(ctx, "gh", "", Page{Limit: 1, After: "gh/alice"})
			if more || len(list) != 1 || list[0].Subject != "bob" {
				t.Fatalf("page 2 = %+v", list)
			}
			if n, _ := s.CountMCPConnections(ctx); n["gh"] != 2 {
				t.Fatalf("count = %v", n)
			}
			if err := s.DeleteMCPConnection(ctx, "gh", "bob"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteMCPConnection(ctx, "gh", "bob"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second delete = %v", err)
			}

			// Connect requests open once and are taken once.
			now := time.Now()
			r := &ConnectRequest{IDHash: "id1", Server: "gh", Subject: "alice", VirtualServer: "dev", Verifier: "v", ExpiresAt: now.Add(time.Minute)}
			if err := s.CreateConnectRequest(ctx, r); err != nil {
				t.Fatal(err)
			}
			if got, err := s.GetConnectRequest(ctx, "id1", now); err != nil || got.Subject != "alice" || got.Verifier != "v" {
				t.Fatalf("get request = %+v, %v", got, err)
			}
			if _, err := s.GetConnectRequest(ctx, "id1", now.Add(2*time.Minute)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("expired request = %v", err)
			}
			if err := s.OpenConnectRequest(ctx, "id1", "st1", now); err != nil {
				t.Fatal(err)
			}
			if err := s.OpenConnectRequest(ctx, "id1", "st2", now); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second open = %v", err)
			}
			if _, err := s.GetConnectRequest(ctx, "id1", now); !errors.Is(err, ErrNotFound) {
				t.Fatalf("an opened request is still offered: %v", err)
			}
			if got, err := s.TakeConnectRequest(ctx, "st1", now); err != nil || got.Server != "gh" {
				t.Fatalf("take = %+v, %v", got, err)
			}
			if _, err := s.TakeConnectRequest(ctx, "st1", now); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second take = %v", err)
			}
			_ = s.CreateConnectRequest(ctx, &ConnectRequest{IDHash: "id2", Server: "gh", Subject: "x", ExpiresAt: now.Add(-time.Second)})
			if n, err := s.PruneConnectRequests(ctx, now); err != nil || n != 1 {
				t.Fatalf("pruned %d, %v", n, err)
			}
		})
	}
}
