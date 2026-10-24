package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

// videoUpstream serves OpenAI's video job API and records the paths it
// was sent.
type videoUpstream struct {
	*httptest.Server
	mu    sync.Mutex
	paths []string
	fail  bool // answer creates with 500
}

func newVideoUpstream(t *testing.T, name string) *videoUpstream {
	t.Helper()
	u := &videoUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.paths = append(u.paths, r.Method+" "+r.URL.RequestURI())
		fail := u.fail
		u.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer key-"+name {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST" && r.URL.Path == "/videos":
			if fail {
				http.Error(w, `{"error":{"message":"down"}}`, http.StatusInternalServerError)
				return
			}
			model, prompt := "", ""
			if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
				model, prompt = r.FormValue("model"), r.FormValue("prompt")
				if _, _, err := r.FormFile("input_reference"); err != nil {
					t.Errorf("multipart create without its file: %v", err)
				}
			} else {
				var b struct{ Model, Prompt string }
				_ = json.NewDecoder(r.Body).Decode(&b)
				model, prompt = b.Model, b.Prompt
			}
			io.WriteString(w, `{"id":"video_`+name+`_`+strings.ReplaceAll(prompt, " ", "_")+`","object":"video","model":"`+model+`","status":"queued","seconds":"8"}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/remix"):
			io.WriteString(w, `{"id":"video_`+name+`_remix","object":"video","status":"queued","seconds":"4","remixed_from_video_id":"`+strings.Split(r.URL.Path, "/")[2]+`"}`)
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/content"):
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("MP4DATA"))
		case r.Method == "GET":
			io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/videos/")+`","object":"video","status":"completed","seconds":"8"}`)
		case r.Method == "DELETE":
			io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/videos/")+`","object":"video","deleted":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *videoUpstream) calls() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func TestVideoJobs(t *testing.T) {
	first, second := newVideoUpstream(t, "first"), newVideoUpstream(t, "second")
	first.fail = true
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
providers:
  first: {base_url: "`+first.URL+`", api_key: key-first}
  second: {base_url: "`+second.URL+`", api_key: key-second}
models:
  video: {targets: [{provider: first, model: sora-2}, {provider: second, model: sora-2-pro, price: {per_video_second: 0.5}}]}
keys:
  - {name: static-a, key: `+devKey+`}
  - {name: static-b, key: `+guardKey+`}
`)
	var org, team, other, key, otherKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"film"}`, 201, &team)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"other"}`, 201, &other)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"a"}`, 201, &key)
	e.call("POST", "/admin/v1/teams/"+other.ID+"/keys", `{"name":"b"}`, 201, &otherKey)

	// Created on the second target after the first fails, and charged.
	rec := do(e.h, "POST", "/v1/videos", key.Key, `{"model":"video","prompt":"a falcon","seconds":"8"}`)
	if rec.Code != 200 || rec.Header().Get("X-Gateway-Target") != "second/sora-2-pro" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	var job struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if job.ID != "video_second_a_falcon" {
		t.Fatalf("job = %s", rec.Body)
	}

	// Later calls go to the target that made the job, never the first.
	if rec := do(e.h, "GET", "/v1/videos/"+job.ID, key.Key, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "completed") {
		t.Fatalf("retrieve = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/v1/videos/"+job.ID+"/content?variant=thumbnail&x=1", key.Key, ""); rec.Code != 200 || rec.Body.String() != "MP4DATA" || rec.Header().Get("Content-Type") != "video/mp4" {
		t.Fatalf("content = %d %q", rec.Code, rec.Body)
	}
	rec = do(e.h, "POST", "/v1/videos/"+job.ID+"/remix", key.Key, `{"prompt":"at night"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"remixed_from_video_id":"`+job.ID+`"`) {
		t.Fatalf("remix = %d %s", rec.Code, rec.Body)
	}
	for _, c := range first.calls() {
		if c != "POST /videos" {
			t.Fatalf("the first target was sent %q", c)
		}
	}
	got := strings.Join(second.calls(), "\n")
	for _, want := range []string{"GET /videos/" + job.ID, "GET /videos/" + job.ID + "/content?variant=thumbnail", "POST /videos/" + job.ID + "/remix"} {
		if !strings.Contains(got, want+"\n") && !strings.HasSuffix(got, want) {
			t.Fatalf("second target calls = %s, want %q", got, want)
		}
	}

	// Listing shows the team's jobs, newest first, as last stored.
	var list struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
		LastID  string           `json:"last_id"`
	}
	if err := json.Unmarshal(do(e.h, "GET", "/v1/videos?limit=1", key.Key, "").Body.Bytes(), &list); err != nil || len(list.Data) != 1 || !list.HasMore {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := json.Unmarshal(do(e.h, "GET", "/v1/videos?after="+list.LastID, key.Key, "").Body.Bytes(), &list); err != nil || len(list.Data) != 1 || list.Data[0]["status"] != "completed" {
		t.Fatalf("second page = %+v, %v", list, err)
	}

	// Another team cannot see, fetch, remix or delete it.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/videos/" + job.ID, ""},
		{"GET", "/v1/videos/" + job.ID + "/content", ""},
		{"DELETE", "/v1/videos/" + job.ID, ""},
		{"POST", "/v1/videos/" + job.ID + "/remix", `{"prompt":"x"}`},
		{"GET", "/v1/videos/video_unknown", ""},
	} {
		if rec := do(e.h, c.method, c.path, otherKey.Key, c.body); rec.Code != 404 || errorCode(t, rec) != codeNotFound {
			t.Fatalf("%s %s by another team = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if rec := do(e.h, "GET", "/v1/videos", otherKey.Key, ""); !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Fatalf("other team's list = %s", rec.Body)
	}

	// The create and the remix are charged by the seconds they make;
	// reads are not.
	e.srv.Close(context.Background())
	var usage struct {
		Data []store.UsageRow `json:"data"`
	}
	e.call("GET", "/admin/v1/usage?org_id="+org.ID+"&group_by=endpoint", "", 200, &usage)
	costs := map[string]float64{}
	for _, row := range usage.Data {
		costs[row.Group] = row.CostUSD
		if row.Group == "videos" && (row.Requests != 1 || row.VideoSeconds != 8) {
			t.Fatalf("videos usage = %+v", row)
		}
	}
	if len(costs) != 2 || costs["videos"] != 4 || costs["videos.remix"] != 2 {
		t.Fatalf("usage = %+v", usage.Data)
	}

	// Static keys share a team but not their jobs.
	rec = do(e.h, "POST", "/v1/videos", devKey, `{"model":"video","prompt":"static"}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &job)
	if rec := do(e.h, "GET", "/v1/videos/"+job.ID, guardKey, ""); rec.Code != 404 {
		t.Fatalf("other static key = %d", rec.Code)
	}
	if rec := do(e.h, "GET", "/v1/videos/"+job.ID, devKey, ""); rec.Code != 200 {
		t.Fatalf("own static key = %d %s", rec.Code, rec.Body)
	}

	// Delete removes the record.
	if rec := do(e.h, "DELETE", "/v1/videos/"+job.ID, devKey, ""); rec.Code != 200 {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/v1/videos/"+job.ID, devKey, ""); rec.Code != 404 {
		t.Fatalf("after delete = %d", rec.Code)
	}
}

func TestVideoMultipartAndUsage(t *testing.T) {
	up := newVideoUpstream(t, "up")
	e := newAdminEnvConfig(t, `
store: {driver: sqlite, dsn: unused}
providers: {up: {base_url: "`+up.URL+`", api_key: key-up}}
models:
  video: {targets: [{provider: up, model: sora-2, price: {per_video_second: 0.1}}]}
keys: [{name: dev, key: `+devKey+`}]
`)
	body, ct := multipartBody(t, map[string]string{"model": "video", "prompt": "ref"}, "PNG")
	body = strings.Replace(body, `name="file"`, `name="input_reference"`, 1)
	if rec := doType(e.h, "/v1/videos", devKey, body, ct); rec.Code != 200 || !strings.Contains(rec.Body.String(), "video_up_ref") {
		t.Fatalf("multipart create = %d %s", rec.Code, rec.Body)
	}
}

func TestVideosWithoutDatabase(t *testing.T) {
	up := newVideoUpstream(t, "up")
	h := gateway(t, `
providers: {up: {base_url: "`+up.URL+`", api_key: key-up}}
models: {video: {targets: [{provider: up, model: sora-2}]}}
keys: [{name: dev, key: `+devKey+`}]
`, nil)
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/videos", `{"model":"video","prompt":"x"}`},
		{"GET", "/v1/videos", ""},
		{"GET", "/v1/videos/video_1", ""},
		{"POST", "/v1/videos/video_1/remix", `{"prompt":"x"}`},
	} {
		if rec := do(h, c.method, c.path, devKey, c.body); rec.Code != 501 || errorCode(t, rec) != codeNoDatabase {
			t.Fatalf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if len(up.calls()) != 0 {
		t.Fatalf("upstream was called: %v", up.calls())
	}
}
