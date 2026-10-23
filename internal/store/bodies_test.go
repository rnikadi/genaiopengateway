package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/usage"
)

func TestRequestBodies(t *testing.T) {
	ctx := context.Background()
	for name, s := range backends(t) {
		t.Run(name, func(t *testing.T) {
			old := time.Now().Add(-48 * time.Hour)
			var bodies []*RequestBody
			for i := range pruneBatch + 5 {
				bodies = append(bodies, &RequestBody{RequestID: fmt.Sprintf("old-%d", i), Time: old, OrgID: "org_a", Kind: "model", Request: "x", Response: "y"})
			}
			// The same request id in two organizations: clients choose ids.
			bodies = append(bodies,
				&RequestBody{RequestID: "req-1", Time: time.Now(), OrgID: "org_a", Kind: "model", Request: "a", Response: "a", Truncated: true},
				&RequestBody{RequestID: "req-1", Time: time.Now().Add(time.Second), OrgID: "org_b", Kind: "model", Request: "b", Response: "b"})
			if err := s.InsertRequestBodies(ctx, bodies); err != nil {
				t.Fatal(err)
			}
			got, err := s.RequestBodies(ctx, "req-1")
			if err != nil || len(got) != 2 || got[0].OrgID != "org_b" || !got[1].Truncated || string(got[1].Redactions) != "{}" {
				t.Fatalf("RequestBodies = %+v, %v", got, err)
			}

			now := time.Now()
			if err := s.InsertUsage(ctx, []usage.Record{
				{RequestID: "req-0", Time: now.Add(-time.Minute), OrgID: "org_a"},
				{RequestID: "req-1", Time: now, OrgID: "org_a"},
				{RequestID: "req-1", Time: now, OrgID: "org_b"},
				{RequestID: "req-2", Time: now, OrgID: "org_a"},
			}); err != nil {
				t.Fatal(err)
			}
			rows, more, err := s.ListRequests(ctx, RequestQuery{OrgID: "org_a"}, Page{Limit: 2})
			if err != nil || !more || len(rows) != 2 || rows[0].RequestID != "req-2" || rows[0].HasBody || !rows[1].HasBody {
				t.Fatalf("page 1 = %+v, %v, %v", rows, more, err)
			}
			rows, more, err = s.ListRequests(ctx, RequestQuery{OrgID: "org_a"}, Page{Limit: 2, After: rows[1].RequestID})
			if err != nil || more || len(rows) != 1 || rows[0].RequestID != "req-0" {
				t.Fatalf("page 2 = %+v, %v, %v", rows, more, err)
			}

			n, err := s.PruneRequestBodies(ctx, time.Now().Add(-24*time.Hour))
			if err != nil || n != pruneBatch+5 {
				t.Fatalf("pruned %d, %v", n, err)
			}
			n, err = s.DeleteRequestBodies(ctx, "org_b", time.Now().Add(time.Hour))
			if err != nil || n != 1 {
				t.Fatalf("deleted %d, %v", n, err)
			}
			if got, _ := s.RequestBodies(ctx, "req-1"); len(got) != 1 || got[0].OrgID != "org_a" {
				t.Fatalf("left = %+v", got)
			}
		})
	}
}
