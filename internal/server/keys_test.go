package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Virtual keys (docs/virtual-keys.md): listing across teams, owners and
// rotation with a grace period.
func TestVirtualKeys(t *testing.T) {
	e := newAdminEnv(t, "")
	var acme, other, search, ops, key, otherKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &acme)
	e.call("POST", "/admin/v1/orgs", `{"name":"other"}`, 201, &other)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"search"}`, 201, &search)
	e.call("POST", "/admin/v1/orgs/"+other.ID+"/teams", `{"name":"ops"}`, 201, &ops)
	e.call("POST", "/admin/v1/teams/"+search.ID+"/keys", `{"name":"indexer","owner":" dana@example.com "}`, 201, &key)
	e.call("POST", "/admin/v1/teams/"+ops.ID+"/keys", `{"name":"pager"}`, 201, &otherKey)
	e.call("POST", "/admin/v1/orgs/"+acme.ID+"/teams", `{"name":"x","owner":"y"}`, 400, nil)

	list := func(query string) []store.Key {
		var out struct{ Data []store.Key }
		e.call("GET", "/admin/v1/keys"+query, "", 200, &out)
		return out.Data
	}
	if got := list(""); len(got) != 2 {
		t.Fatalf("all keys = %+v", got)
	}
	if got := list("?q=DANA"); len(got) != 1 || got[0].Owner != "dana@example.com" || got[0].TeamID != search.ID {
		t.Errorf("search by owner = %+v", got)
	}
	if got := list("?org_id=" + other.ID); len(got) != 1 || got[0].ID != otherKey.ID {
		t.Errorf("by org = %+v", got)
	}
	e.call("GET", "/admin/v1/keys?status=nope", "", 400, nil)

	// Rotation: both secrets work during the grace period.
	var rotated idResp
	e.call("POST", "/admin/v1/keys/"+key.ID+"/rotate", `{"grace":"1h"}`, 200, &rotated)
	if rotated.ID != key.ID || rotated.Key == key.Key || !strings.HasPrefix(rotated.Key, "sk-gog-") {
		t.Fatalf("rotated = %+v", rotated)
	}
	for _, secret := range []string{key.Key, rotated.Key} {
		if rec := do(e.h, "POST", "/v1/chat/completions", secret, `{"model":"chat"}`); rec.Code != 200 {
			t.Errorf("during the grace period = %d %s", rec.Code, rec.Body)
		}
	}
	if got := list("?status=rotating"); len(got) != 1 || got[0].PreviousExpiresAt == nil {
		t.Errorf("rotating = %+v", got)
	}
	// A rotated secret is not reported as an expiring key.
	e.srv.checkKeyExpiry(t.Context(), time.Now())
	if alerts, _, _ := e.srv.store.ListAlerts(t.Context(), "", "key.expiring", store.Page{}); len(alerts) != 0 {
		t.Errorf("expiring alerts for a rotation = %+v", alerts)
	}

	// Rotating again with no grace stops both earlier secrets at once.
	var again idResp
	e.call("POST", "/admin/v1/keys/"+key.ID+"/rotate", `{"grace":"0"}`, 200, &again)
	for _, secret := range []string{key.Key, rotated.Key} {
		if rec := do(e.h, "POST", "/v1/chat/completions", secret, `{"model":"chat"}`); rec.Code != 401 {
			t.Errorf("old secret after a second rotation = %d", rec.Code)
		}
	}
	if rec := do(e.h, "POST", "/v1/chat/completions", again.Key, `{"model":"chat"}`); rec.Code != 200 {
		t.Errorf("new secret = %d", rec.Code)
	}
	for _, body := range []string{`{"grace":"31d"}`, `{"grace":"-1h"}`, `{"grace":"soon"}`} {
		e.call("POST", "/admin/v1/keys/"+key.ID+"/rotate", body, 400, nil)
	}

	// Revoking stops the key; a revoked key cannot be rotated.
	e.call("POST", "/admin/v1/keys/"+key.ID+"/rotate", `{}`, 200, &rotated)
	e.call("POST", "/admin/v1/keys/"+key.ID+"/revoke", "", 200, nil)
	for _, secret := range []string{again.Key, rotated.Key} {
		if rec := do(e.h, "POST", "/v1/chat/completions", secret, `{"model":"chat"}`); rec.Code != 401 {
			t.Errorf("revoked = %d", rec.Code)
		}
	}
	e.call("POST", "/admin/v1/keys/"+key.ID+"/rotate", `{}`, http.StatusConflict, nil)
	if got := list("?status=revoked"); len(got) != 1 || got[0].ID != key.ID {
		t.Errorf("revoked = %+v", got)
	}

	// An organization admin sees only their organization's keys.
	lead := e.session("lee@example.com", store.Grant{Role: config.RoleAdmin, OrgID: other.ID})
	rec := e.browser(lead, "GET", "/admin/v1/keys", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), key.ID) || !strings.Contains(rec.Body.String(), otherKey.ID) {
		t.Errorf("org admin's keys = %d %s", rec.Code, rec.Body)
	}
	if rec := e.browser(lead, "POST", "/admin/v1/keys/"+key.ID+"/rotate", `{}`); rec.Code != 403 {
		t.Errorf("rotating another organization's key = %d", rec.Code)
	}
	nobody := e.session("new@example.com")
	if rec := e.browser(nobody, "GET", "/admin/v1/keys", ""); rec.Code != 200 || strings.Contains(rec.Body.String(), "key_") {
		t.Errorf("no grants = %d %s", rec.Code, rec.Body)
	}
	audit, _, _ := e.srv.store.ListAudit(t.Context(), acme.ID, store.Page{})
	var rotations int
	for _, a := range audit {
		if a.Action == "key.rotate" {
			rotations++
		}
	}
	if rotations != 3 {
		t.Errorf("key.rotate audited %d times, want 3", rotations)
	}
}
