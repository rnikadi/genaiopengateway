package store

import (
	"context"
	"errors"
	"testing"
)

func TestSCIMDirectory(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			ada := &SCIMUser{UserName: "Ada@example.com", Email: "ADA@example.com", Active: true}
			bob := &SCIMUser{UserName: "bob", Email: "bob@example.com", Active: true}
			for _, u := range []*SCIMUser{ada, bob} {
				if err := s.CreateSCIMUser(ctx, u); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.CreateSCIMUser(ctx, &SCIMUser{UserName: "ada@EXAMPLE.com"}); !errors.Is(err, ErrConflict) {
				t.Fatalf("duplicate user name: %v", err)
			}
			g := &SCIMGroup{DisplayName: "Owners"}
			if err := s.CreateSCIMGroup(ctx, g, []string{ada.ID}); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSCIMGroup(ctx, &SCIMGroup{DisplayName: "owners"}, nil); !errors.Is(err, ErrConflict) {
				t.Fatalf("duplicate group name: %v", err)
			}
			if err := s.CreateSCIMGroup(ctx, &SCIMGroup{DisplayName: "x"}, []string{"usr_missing"}); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown member: %v", err)
			}

			found, err := s.FindSCIMUser(ctx, "bob@example.com")
			if err != nil || found.ID != bob.ID {
				t.Fatalf("find by email: %+v %v", found, err)
			}
			found, err = s.FindSCIMUser(ctx, "ada@example.com")
			if err != nil || found.ID != ada.ID || len(found.Groups) != 1 || found.Groups[0].Display != "Owners" {
				t.Fatalf("find by user name: %+v %v", found, err)
			}

			users, total, err := s.ListSCIMUsers(ctx, SCIMFilter{Attr: "user_name", Value: "ADA@example.com"}, 0, 10)
			if err != nil || total != 1 || users[0].ID != ada.ID {
				t.Fatalf("filter users: %v %d %v", users, total, err)
			}
			users, total, err = s.ListSCIMUsers(ctx, SCIMFilter{}, 1, 10)
			if err != nil || total != 2 || len(users) != 1 {
				t.Fatalf("page users: %v %d %v", users, total, err)
			}

			g.DisplayName = "Admins"
			if err := s.UpdateSCIMGroup(ctx, g, nil, []string{bob.ID}, []string{ada.ID}); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetSCIMGroup(ctx, g.ID)
			if err != nil || got.DisplayName != "Admins" || len(got.Members) != 1 || got.Members[0].ID != bob.ID {
				t.Fatalf("updated group: %+v %v", got, err)
			}
			if err := s.UpdateSCIMGroup(ctx, g, []string{ada.ID, bob.ID}, nil, nil); err != nil {
				t.Fatal(err)
			}
			groups, total, err := s.ListSCIMGroups(ctx, SCIMFilter{Attr: "display_name", Value: "admins"}, 0, 10, true)
			if err != nil || total != 1 || len(groups[0].Members) != 2 {
				t.Fatalf("filter groups: %v %d %v", groups, total, err)
			}

			ada.Active = false
			if err := s.UpdateSCIMUser(ctx, ada); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteSCIMUser(ctx, bob.ID); err != nil {
				t.Fatal(err)
			}
			got, _ = s.GetSCIMGroup(ctx, g.ID)
			if len(got.Members) != 1 {
				t.Fatalf("deleting a user removes its memberships: %+v", got.Members)
			}
			if err := s.DeleteSCIMGroup(ctx, g.ID); err != nil {
				t.Fatal(err)
			}
			if found, _ := s.FindSCIMUser(ctx, "ada@example.com"); found.Active || len(found.Groups) != 0 {
				t.Fatalf("after deleting the group: %+v", found)
			}
		})
	}
}
