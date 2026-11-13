package store

import (
	"context"
	"errors"
	"testing"
)

func TestConfigObjects(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			rev0, err := s.ConfigRevision(ctx)
			if err != nil {
				t.Fatal(err)
			}
			o := &ConfigObject{Kind: "providers", Name: "openai", Spec: `{"type":"openai"}`, Secrets: "sealed", UpdatedBy: "owner"}
			if created, err := s.PutConfigObject(ctx, o); err != nil || !created {
				t.Fatalf("put = %v, %v", created, err)
			}
			o.Spec = `{"type":"openai","timeout":"5s"}`
			if created, err := s.PutConfigObject(ctx, o); err != nil || created {
				t.Fatalf("replace = %v, %v", created, err)
			}
			got, err := s.GetConfigObject(ctx, "providers", "openai")
			if err != nil || got.Spec != o.Spec || got.Secrets != "sealed" || got.UpdatedBy != "owner" || got.UpdatedAt.IsZero() {
				t.Fatalf("get = %+v, %v", got, err)
			}
			list, rev, err := s.ListConfigObjects(ctx)
			if err != nil || len(list) != 1 || rev != rev0+2 {
				t.Fatalf("list = %d objects at %d (from %d), %v", len(list), rev, rev0, err)
			}
			if err := s.DeleteConfigObject(ctx, "providers", "openai"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteConfigObject(ctx, "providers", "openai"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second delete = %v", err)
			}
			if _, err := s.GetConfigObject(ctx, "providers", "openai"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get after delete = %v", err)
			}
			if rev, _ := s.ConfigRevision(ctx); rev != rev0+3 {
				t.Fatalf("revision = %d, want %d", rev, rev0+3)
			}
		})
	}
}
