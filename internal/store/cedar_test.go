package store

import (
	"context"
	"errors"
	"testing"
)

func TestCedarPolicyVersions(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a, b := &Org{Name: "a-" + name}, &Org{Name: "b-" + name}
			for _, o := range []*Org{a, b} {
				if err := s.CreateOrg(ctx, o); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.GetCedarPolicies(ctx, a.ID, 0); !errors.Is(err, ErrNotFound) {
				t.Fatalf("none yet: %v", err)
			}
			if _, err := s.SaveCedarPolicies(ctx, "org_missing", "x", "t"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown org: %v", err)
			}
			for _, text := range []string{"one", "two"} {
				if _, err := s.SaveCedarPolicies(ctx, a.ID, text, "ada"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.SaveCedarPolicies(ctx, b.ID, "b", "bob"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.SaveCedarPolicies(ctx, b.ID, "", "bob"); err != nil {
				t.Fatal(err)
			}
			cur, err := s.GetCedarPolicies(ctx, a.ID, 0)
			if err != nil || cur.Version != 2 || cur.Text != "two" || cur.CreatedBy != "ada" {
				t.Fatalf("current: %+v %v", cur, err)
			}
			if v1, err := s.GetCedarPolicies(ctx, a.ID, 1); err != nil || v1.Text != "one" {
				t.Fatalf("version 1: %+v %v", v1, err)
			}
			all, err := s.LoadCedarPolicies(ctx)
			if err != nil || all[a.ID] == nil || all[a.ID].Text != "two" || all[b.ID] != nil {
				t.Fatalf("load: %+v %v", all, err)
			}
			versions, err := s.ListCedarVersions(ctx, a.ID, 10)
			if err != nil || len(versions) != 2 || versions[0].Version != 2 || versions[0].Text != "" {
				t.Fatalf("versions: %+v %v", versions, err)
			}
			if err := s.DeleteOrg(ctx, b.ID); err != nil {
				t.Fatal(err)
			}
			if v, _ := s.ListCedarVersions(ctx, b.ID, 10); len(v) != 0 {
				t.Fatalf("deleted org's versions remain: %+v", v)
			}
		})
	}
}
