package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestVideoJobs(t *testing.T) {
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, j := range []*VideoJob{
				{ID: "video_a", OrgID: "org", TeamID: "team", KeyID: "k1", Model: "video", Provider: "openai", UpstreamModel: "sora-2", Object: json.RawMessage(`{"id":"video_a"}`)},
				{ID: "video_b", OrgID: "org", TeamID: "team", KeyID: "k2", Model: "video", Provider: "openai", UpstreamModel: "sora-2", Object: json.RawMessage(`{"id":"video_b"}`)},
				{ID: "video_c", OrgID: "org", TeamID: "other", KeyID: "k3", Model: "video", Provider: "openai", UpstreamModel: "sora-2", Object: json.RawMessage(`{}`)},
			} {
				if err := s.CreateVideoJob(ctx, j); err != nil {
					t.Fatal(err)
				}
			}
			// A repeated id keeps its first owner.
			if err := s.CreateVideoJob(ctx, &VideoJob{ID: "video_a", OrgID: "org", TeamID: "other", Object: json.RawMessage(`{}`)}); err != nil {
				t.Fatal(err)
			}
			if j, err := s.GetVideoJob(ctx, "video_a"); err != nil || j.TeamID != "team" || j.UpstreamModel != "sora-2" {
				t.Fatalf("get = %+v, %v", j, err)
			}
			if err := s.UpdateVideoJob(ctx, "video_a", json.RawMessage(`{"id":"video_a","status":"completed"}`)); err != nil {
				t.Fatal(err)
			}
			if j, _ := s.GetVideoJob(ctx, "video_a"); string(j.Object) != `{"id":"video_a","status":"completed"}` {
				t.Fatalf("object = %s", j.Object)
			}

			page, more, err := s.ListVideoJobs(ctx, "org", "team", "", Page{Limit: 1})
			if err != nil || len(page) != 1 || !more {
				t.Fatalf("first page = %d, %v, %v", len(page), more, err)
			}
			rest, more, err := s.ListVideoJobs(ctx, "org", "team", "", Page{Limit: 5, After: page[0].ID})
			if err != nil || len(rest) != 1 || more || rest[0].ID == page[0].ID {
				t.Fatalf("second page = %+v, %v, %v", rest, more, err)
			}
			if mine, _, _ := s.ListVideoJobs(ctx, "org", "team", "k2", Page{}); len(mine) != 1 || mine[0].ID != "video_b" {
				t.Fatalf("by key = %+v", mine)
			}

			if err := s.DeleteVideoJob(ctx, "video_a"); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetVideoJob(ctx, "video_a"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("get after delete = %v", err)
			}
		})
	}
}
