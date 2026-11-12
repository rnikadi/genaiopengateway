package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDenials(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			now := time.Now().UTC()
			ds := []Denial{
				{RequestID: "req-1", Time: now.Add(-48 * time.Hour), OrgID: "org_a", KeyID: "key_1", Model: "chat", Status: 403, Code: "model_not_allowed", Reason: "no"},
				{RequestID: "req-2", Time: now, OrgID: "org_a", TeamID: "team_a", KeyID: "key_1", Model: "chat", Status: 429, Code: "rate_limit_exceeded"},
				{RequestID: "req-3", Time: now.Add(time.Millisecond), OrgID: "org_b", KeyID: "key_2", Model: "chat", Status: 402, Code: "budget_exceeded"},
			}
			if err := s.InsertDenials(ctx, ds); err != nil {
				t.Fatal(err)
			}
			// A retried batch is harmless.
			if err := s.InsertDenials(ctx, ds[:1]); err != nil {
				t.Fatal(err)
			}
			got, more, err := s.ListDenials(ctx, DenialQuery{OrgID: "org_a"}, Page{})
			if err != nil || more || len(got) != 2 || got[0].RequestID != "req-2" || got[1].Reason != "no" {
				t.Fatalf("list = %+v, %v", got, err)
			}
			got, more, _ = s.ListDenials(ctx, DenialQuery{}, Page{Limit: 1})
			if len(got) != 1 || !more || got[0].RequestID != "req-3" {
				t.Fatalf("page 1 = %+v %v", got, more)
			}
			got, _, _ = s.ListDenials(ctx, DenialQuery{}, Page{Limit: 1, After: "req-3"})
			if len(got) != 1 || got[0].RequestID != "req-2" {
				t.Fatalf("page 2 = %+v", got)
			}
			d, err := s.GetDenial(ctx, "req-3")
			if err != nil || d.Code != "budget_exceeded" || d.Status != 402 {
				t.Fatalf("get = %+v, %v", d, err)
			}
			if _, err := s.GetDenial(ctx, "req-x"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing = %v", err)
			}
			if n, err := s.PruneDenials(ctx, now.Add(-time.Hour)); err != nil || n != 1 {
				t.Fatalf("prune = %d, %v", n, err)
			}
		})
	}
}
