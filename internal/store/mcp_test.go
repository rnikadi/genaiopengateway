package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/id"
)

func TestMCPToolPins(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			def := json.RawMessage(`{"name":"search"}`)
			if err := s.InsertPin(ctx, &MCPToolPin{Server: "gh", Tool: "search", Hash: "h1", ApprovedHash: "h1", Definition: def}); err != nil {
				t.Fatal(err)
			}
			// Another replica racing to insert a different first sight loses.
			if err := s.InsertPin(ctx, &MCPToolPin{Server: "gh", Tool: "search", Hash: "h9", ApprovedHash: "h9", Definition: def}); err != nil {
				t.Fatal(err)
			}
			if err := s.InsertPin(ctx, &MCPToolPin{Server: "gh", Tool: "new", Hash: "n1", Definition: def}); err != nil {
				t.Fatal(err)
			}
			p, err := s.GetPin(ctx, "gh", "search")
			if err != nil || p.Hash != "h1" || p.Status() != PinApproved {
				t.Fatalf("pin %+v, %v", p, err)
			}
			if p, _ := s.GetPin(ctx, "gh", "new"); p.Status() != PinPending {
				t.Fatalf("a pin without approval is %s", p.Status())
			}

			if err := s.UpdatePinHash(ctx, &MCPToolPin{Server: "gh", Tool: "search", Hash: "h2", Definition: json.RawMessage(`{"name":"search","description":"evil"}`)}); err != nil {
				t.Fatal(err)
			}
			if p, _ := s.GetPin(ctx, "gh", "search"); p.Status() != PinChanged || p.ApprovedHash != "h1" || string(p.Definition) != `{"name":"search","description":"evil"}` {
				t.Fatalf("changed pin %+v", p)
			}
			if _, err := s.ApprovePin(ctx, "gh", "search", "h1"); !errors.Is(err, ErrConflict) {
				t.Fatalf("approving a stale hash: %v, want conflict", err)
			}
			if _, err := s.ApprovePin(ctx, "gh", "nope", "h1"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("approving an unknown tool: %v, want not found", err)
			}
			p, err = s.ApprovePin(ctx, "gh", "search", "h2")
			if err != nil || p.Status() != PinApproved {
				t.Fatalf("approve: %+v, %v", p, err)
			}
			all, err := s.ListPins(ctx, "")
			if err != nil || len(all) != 2 || all[0].Tool != "new" {
				t.Fatalf("list %+v, %v", all, err)
			}
		})
	}
}

func TestMCPCalls(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			base := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
			var calls []MCPCall
			for i, org := range []string{"o1", "o1", "o2"} {
				calls = append(calls, MCPCall{
					ID: id.New("mcpcall"), Time: base.Add(time.Duration(i) * time.Minute), OrgID: org, TeamID: "t", KeyID: "k",
					VirtualServer: "eng", Server: "gh", Method: "tools/call", Name: "search", Status: "ok", LatencyMS: 12, RequestID: "req",
				})
				time.Sleep(2 * time.Millisecond) // ids sort by time
			}
			if err := s.InsertMCPCalls(ctx, calls); err != nil {
				t.Fatal(err)
			}
			got, more, err := s.ListMCPCalls(ctx, MCPCallQuery{OrgID: "o1"}, Page{Limit: 1})
			if err != nil || len(got) != 1 || !more || got[0].ID != calls[1].ID || !got[0].Time.Equal(calls[1].Time) {
				t.Fatalf("first page %+v %v %v", got, more, err)
			}
			got, more, err = s.ListMCPCalls(ctx, MCPCallQuery{OrgID: "o1"}, Page{Limit: 1, After: got[0].ID})
			if err != nil || len(got) != 1 || more || got[0].ID != calls[0].ID {
				t.Fatalf("second page %+v %v %v", got, more, err)
			}
			got, _, _ = s.ListMCPCalls(ctx, MCPCallQuery{From: base.Add(90 * time.Second)}, Page{})
			if len(got) != 1 || got[0].OrgID != "o2" {
				t.Fatalf("from filter %+v", got)
			}
			if n, err := s.PruneMCPCalls(ctx, base.Add(30*time.Second)); err != nil || n != 1 {
				t.Fatalf("prune %d, %v", n, err)
			}
		})
	}
}
