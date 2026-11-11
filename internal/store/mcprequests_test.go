package store

import (
	"context"
	"errors"
	"testing"
)

func TestMCPAccessRequests(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			o := &Org{Name: "o-" + name}
			if err := s.CreateOrg(ctx, o); err != nil {
				t.Fatal(err)
			}
			a, b := &Team{OrgID: o.ID, Name: "a"}, &Team{OrgID: o.ID, Name: "b"}
			for _, tm := range []*Team{a, b} {
				if err := s.CreateTeam(ctx, tm); err != nil {
					t.Fatal(err)
				}
			}
			ra := &MCPAccessRequest{OrgID: o.ID, TeamID: a.ID, Server: "docs", Reason: "r", RequestedBy: "ada"}
			rb := &MCPAccessRequest{OrgID: o.ID, TeamID: b.ID, Server: "docs", RequestedBy: "bob"}
			for _, r := range []*MCPAccessRequest{ra, rb} {
				if err := s.CreateMCPAccessRequest(ctx, r); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CreateMCPAccessRequest(ctx, &MCPAccessRequest{OrgID: o.ID, TeamID: a.ID, Server: "docs"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("second pending: %v", err)
			}
			got, err := s.ListMCPAccessRequests(ctx, MCPRequestFilter{TeamIDs: []string{a.ID}})
			if err != nil || len(got) != 1 || got[0].ID != ra.ID || got[0].DecidedAt != nil {
				t.Fatalf("by team: %+v %v", got, err)
			}
			if got, _ := s.ListMCPAccessRequests(ctx, MCPRequestFilter{OrgIDs: []string{}, TeamIDs: []string{}}); len(got) != 0 {
				t.Fatalf("empty scope: %+v", got)
			}
			d, err := s.DecideMCPAccessRequest(ctx, ra.ID, RequestApproved, "owner", "ok")
			if err != nil || d.Status != RequestApproved || d.DecidedAt == nil || d.Note != "ok" {
				t.Fatalf("decide: %+v %v", d, err)
			}
			if _, err := s.DecideMCPAccessRequest(ctx, ra.ID, RequestDenied, "owner", ""); !errors.Is(err, ErrConflict) {
				t.Fatalf("decide twice: %v", err)
			}
			if _, err := s.DecideMCPAccessRequest(ctx, "mcpreq_missing", RequestDenied, "owner", ""); !errors.Is(err, ErrNotFound) {
				t.Fatalf("decide missing: %v", err)
			}
			if got, _ := s.ListMCPAccessRequests(ctx, MCPRequestFilter{OrgIDs: []string{o.ID}, Status: RequestPending}); len(got) != 1 || got[0].ID != rb.ID {
				t.Fatalf("pending: %+v", got)
			}
			if err := s.DeleteTeam(ctx, b.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetMCPAccessRequest(ctx, rb.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted team's request: %v", err)
			}
		})
	}
}
