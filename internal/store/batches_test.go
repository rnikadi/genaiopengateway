package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestBatchFiles(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, f := range []*BatchFile{
				{ID: "file_a", OrgID: "org", TeamID: "team", KeyID: "k1", Model: "chat", Provider: "openai", UpstreamModel: "gpt", Purpose: "batch", Object: json.RawMessage(`{"id":"file_a"}`)},
				{ID: "file_b", OrgID: "org", TeamID: "team", KeyID: "k2", Model: "chat", Provider: "openai", UpstreamModel: "gpt", Purpose: "batch_output", Object: json.RawMessage(`{"id":"file_b"}`)},
				{ID: "file_c", OrgID: "org", TeamID: "other", KeyID: "k3", Model: "chat", Provider: "openai", UpstreamModel: "gpt", Purpose: "batch", Object: json.RawMessage(`{}`)},
			} {
				if err := s.CreateBatchFile(ctx, f); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CreateBatchFile(ctx, &BatchFile{ID: "file_a", OrgID: "org", TeamID: "other", Object: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if f, err := s.GetBatchFile(ctx, "file_a"); err != nil || f.TeamID != "team" || f.Purpose != "batch" {
				t.Fatalf("get = %+v, %v", f, err)
			}
			page, more, err := s.ListBatchFiles(ctx, "org", "team", "", Page{Limit: 1})
			if err != nil || len(page) != 1 || !more {
				t.Fatalf("first page = %d, %v, %v", len(page), more, err)
			}
			rest, more, err := s.ListBatchFiles(ctx, "org", "team", "", Page{After: page[0].ID})
			if err != nil || len(rest) != 1 || more || rest[0].ID == page[0].ID {
				t.Fatalf("second page = %+v, %v, %v", rest, more, err)
			}
			if mine, _, _ := s.ListBatchFiles(ctx, "org", "team", "k2", Page{}); len(mine) != 1 || mine[0].ID != "file_b" {
				t.Fatalf("by key = %+v", mine)
			}
			if err := s.DeleteBatchFile(ctx, "file_a"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetBatchFile(ctx, "file_a"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get after delete = %v", err)
			}
		})
	}
}

func TestBatches(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, b := range []*Batch{
				{ID: "batch_a", OrgID: "org", TeamID: "team", KeyID: "k1", Model: "chat", Provider: "openai", UpstreamModel: "gpt", InputFileID: "file_a", Status: "validating", Object: json.RawMessage(`{"id":"batch_a"}`)},
				{ID: "batch_b", OrgID: "org", TeamID: "other", KeyID: "k2", Model: "chat", Provider: "openai", UpstreamModel: "gpt", InputFileID: "file_b", Status: "completed", Object: json.RawMessage(`{"id":"batch_b"}`)},
			} {
				if err := s.CreateBatch(ctx, b); err != nil {
					t.Fatal(err)
				}
			}
			if all, _, err := s.ListBatches(ctx, BatchQuery{OrgID: "org"}, Page{}); err != nil || len(all) != 2 {
				t.Fatalf("org list = %d, %v", len(all), err)
			}
			if done, _, _ := s.ListBatches(ctx, BatchQuery{OrgID: "org", Status: "completed"}, Page{}); len(done) != 1 || done[0].ID != "batch_b" {
				t.Fatalf("by status = %+v", done)
			}
			if team, _, _ := s.ListBatches(ctx, BatchQuery{OrgID: "org", TeamID: "team"}, Page{}); len(team) != 1 || team[0].ID != "batch_a" {
				t.Fatalf("by team = %+v", team)
			}

			// Only unfinished batches are polled, and each once per claim.
			due, err := s.ClaimBatchesToPoll(ctx, time.Now().Add(time.Minute), 10)
			if err != nil || len(due) != 1 || due[0].ID != "batch_a" {
				t.Fatalf("due = %+v, %v", due, err)
			}
			if again, _ := s.ClaimBatchesToPoll(ctx, time.Now().Add(-time.Minute), 10); len(again) != 0 {
				t.Fatalf("claimed again = %+v", again)
			}

			if err := s.UpdateBatch(ctx, "batch_a", "completed", json.RawMessage(`{"id":"batch_a","status":"completed"}`)); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.ClaimBatchCharge(ctx, "batch_a", 1.25); !ok || err != nil {
				t.Fatalf("first charge = %v, %v", ok, err)
			}
			if ok, err := s.ClaimBatchCharge(ctx, "batch_a", 9); ok || err != nil {
				t.Fatalf("second charge = %v, %v", ok, err)
			}
			b, err := s.GetBatch(ctx, "batch_a")
			if err != nil || b.Status != "completed" || b.CostUSD != 1.25 || b.ChargedAt == nil || string(b.Object) != `{"id":"batch_a","status":"completed"}` {
				t.Fatalf("get = %+v, %v", b, err)
			}
			if _, err := s.GetBatch(ctx, "missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing = %v", err)
			}
		})
	}
}
