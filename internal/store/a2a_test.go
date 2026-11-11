package store

import (
	"context"
	"errors"
	"testing"
)

func TestA2AObjects(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if err := s.RecordA2AObject(ctx, &A2AObject{Agent: "planner", Kind: A2ATask, ID: "t1", OrgID: "org", TeamID: "team", KeyID: "k1"}); err != nil {
				t.Fatal(err)
			}
			// A repeated id keeps its first owner; the same id at another
			// agent or of another kind is separate.
			for _, o := range []*A2AObject{
				{Agent: "planner", Kind: A2ATask, ID: "t1", OrgID: "org", TeamID: "other", KeyID: "k2"},
				{Agent: "planner", Kind: A2AContext, ID: "t1", OrgID: "org", TeamID: "other", KeyID: "k2"},
				{Agent: "writer", Kind: A2ATask, ID: "t1", OrgID: "org", TeamID: "other", KeyID: "k2"},
			} {
				if err := s.RecordA2AObject(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			if o, err := s.GetA2AObject(ctx, "planner", A2ATask, "t1"); err != nil || o.TeamID != "team" || o.KeyID != "k1" || o.CreatedAt.IsZero() {
				t.Fatalf("get = %+v, %v", o, err)
			}
			if o, err := s.GetA2AObject(ctx, "writer", A2ATask, "t1"); err != nil || o.TeamID != "other" {
				t.Fatalf("other agent = %+v, %v", o, err)
			}
			if _, err := s.GetA2AObject(ctx, "planner", A2ATask, "missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing = %v", err)
			}
		})
	}
}
