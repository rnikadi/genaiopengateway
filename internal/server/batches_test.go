package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/store"
)

// batchUpstream serves OpenAI's files and batch API from memory.
type batchUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	name    string
	files   map[string]string
	status  string // reported by retrieves
	calls   []string
	failing bool // answer uploads with 500
	n       int
}

func newBatchUpstream(t *testing.T, name string) *batchUpstream {
	t.Helper()
	u := &batchUpstream{name: name, files: map[string]string{"file_out": `{"custom_id":"a","response":{"status_code":200}}` + "\n"}, status: "in_progress"}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		u.calls = append(u.calls, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer key-"+name {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case r.Method == "POST" && r.URL.Path == "/files":
			if u.failing {
				http.Error(w, `{"error":{"message":"down"}}`, http.StatusInternalServerError)
				return
			}
			f, h, err := r.FormFile("file")
			if err != nil || r.FormValue("purpose") != "batch" {
				http.Error(w, "bad upload", http.StatusBadRequest)
				return
			}
			data, _ := io.ReadAll(f)
			u.n++
			id := fmt.Sprintf("file_%s_%d", name, u.n)
			u.files[id] = string(data)
			fmt.Fprintf(w, `{"id":%q,"object":"file","purpose":"batch","filename":%q,"bytes":%d}`, id, h.Filename, len(data))
		case r.Method == "GET" && len(parts) == 3 && parts[0] == "files" && parts[2] == "content":
			w.Header().Set("Content-Type", "application/jsonl")
			io.WriteString(w, u.files[parts[1]])
		case parts[0] == "files" && len(parts) == 2:
			fmt.Fprintf(w, `{"id":%q,"object":"file","deleted":%v}`, parts[1], r.Method == "DELETE")
		case r.Method == "POST" && r.URL.Path == "/batches":
			var b struct {
				InputFileID string `json:"input_file_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			u.n++
			fmt.Fprintf(w, `{"id":"batch_%s_%d","object":"batch","status":"validating","input_file_id":%q}`, name, u.n, b.InputFileID)
		case r.Method == "POST" && len(parts) == 3 && parts[2] == "cancel":
			fmt.Fprintf(w, `{"id":%q,"object":"batch","status":"cancelling"}`, parts[1])
		case r.Method == "GET" && len(parts) == 2 && parts[0] == "batches":
			if u.status == "completed" {
				fmt.Fprintf(w, `{"id":%q,"object":"batch","status":"completed","output_file_id":"file_out","request_counts":{"total":2,"completed":2,"failed":0},
					"usage":{"input_tokens":1000000,"output_tokens":500000,"total_tokens":1500000,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`, parts[1])
				return
			}
			fmt.Fprintf(w, `{"id":%q,"object":"batch","status":%q}`, parts[1], u.status)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *batchUpstream) set(f func(u *batchUpstream)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	f(u)
}

func (u *batchUpstream) file(id string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.files[id]
}

// batchFile is a multipart upload of JSONL lines.
func batchFile(t *testing.T, purpose string, lines ...string) (string, string) {
	t.Helper()
	body, ct := multipartBody(t, map[string]string{"purpose": purpose}, strings.Join(lines, "\n")+"\n")
	return strings.Replace(body, `filename="a.mp3"`, `filename="in.jsonl"`, 1), ct
}

func chatLine(id, model, text string) string {
	return `{"custom_id":"` + id + `","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"` + text + `"}]}}`
}

func TestBatches(t *testing.T) {
	first, second := newBatchUpstream(t, "first"), newBatchUpstream(t, "second")
	first.set(func(u *batchUpstream) { u.failing = true })
	e := newAdminEnvConfig(t, `
admin: {token: `+adminToken+`}
store: {driver: sqlite, dsn: unused}
providers:
  first: {base_url: "`+first.URL+`", api_key: key-first}
  second: {base_url: "`+second.URL+`", api_key: key-second}
models:
  chat: {targets: [{provider: first, model: gpt-a}, {provider: second, model: gpt-b, price: {input_per_mtok: 1, output_per_mtok: 2, batch_discount: 0.5}}]}
  other: {targets: [{provider: second, model: gpt-c}]}
`)
	var org, team, other, key, otherKey idResp
	e.call("POST", "/admin/v1/orgs", `{"name":"acme"}`, 201, &org)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"data","allowed_models":["chat"]}`, 201, &team)
	e.call("POST", "/admin/v1/orgs/"+org.ID+"/teams", `{"name":"other"}`, 201, &other)
	e.call("POST", "/admin/v1/teams/"+team.ID+"/keys", `{"name":"a"}`, 201, &key)
	e.call("POST", "/admin/v1/teams/"+other.ID+"/keys", `{"name":"b"}`, 201, &otherKey)

	// Files are checked line by line before anything is uploaded.
	for _, c := range []struct {
		lines []string
		code  int
		want  string
	}{
		{[]string{chatLine("a", "chat", "hi"), chatLine("b", "other", "hi")}, 400, "line 2 (b): every line must use model chat"},
		{[]string{`{"custom_id":"a","method":"POST","url":"/v1/videos","body":{"model":"chat"}}`}, 400, "is not served in batches"},
		{[]string{`{"custom_id":"a","method":"POST","url":"/v1/chat/completions","body":{}}`}, 400, "body.model is required"},
		{[]string{"not json"}, 400, "line 1: not a JSON object"},
		{[]string{chatLine("a", "other", "hi")}, 403, "may not use model other"},
	} {
		body, ct := batchFile(t, "batch", c.lines...)
		if rec := doType(e.h, "/v1/files", key.Key, body, ct); rec.Code != c.code || !strings.Contains(rec.Body.String(), c.want) {
			t.Fatalf("upload %v = %d %s", c.lines, rec.Code, rec.Body)
		}
	}
	if body, ct := batchFile(t, "assistants", chatLine("a", "chat", "hi")); doType(e.h, "/v1/files", key.Key, body, ct).Code != 400 {
		t.Fatal("other purposes are accepted")
	}

	// The file goes to the second target after the first fails, with the
	// model rewritten in every line.
	body, ct := batchFile(t, "batch", chatLine("a", "chat", "one"), chatLine("b", "chat", "two"))
	rec := doType(e.h, "/v1/files", key.Key, body, ct)
	if rec.Code != 200 || rec.Header().Get("X-Gateway-Target") != "second/gpt-b" {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	var file struct{ ID, Filename string }
	_ = json.Unmarshal(rec.Body.Bytes(), &file)
	if got := second.file(file.ID); strings.Count(got, `"model":"gpt-b"`) != 2 || strings.Contains(got, `"model":"chat"`) || file.Filename != "in.jsonl" {
		t.Fatalf("uploaded %q as %+v", got, file)
	}
	if rec := do(e.h, "GET", "/v1/files", key.Key, ""); !strings.Contains(rec.Body.String(), file.ID) {
		t.Fatalf("files = %s", rec.Body)
	}
	if rec := do(e.h, "GET", "/v1/files/"+file.ID+"/content", key.Key, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "gpt-b") {
		t.Fatalf("content = %d %s", rec.Code, rec.Body)
	}

	// Another team cannot reach the file or start a batch from it.
	if rec := do(e.h, "GET", "/v1/files/"+file.ID, otherKey.Key, ""); rec.Code != 404 || errorCode(t, rec) != codeNotFound {
		t.Fatalf("other team's get = %d %s", rec.Code, rec.Body)
	}
	create := `{"input_file_id":"` + file.ID + `","endpoint":"/v1/chat/completions","completion_window":"24h"}`
	if rec := do(e.h, "POST", "/v1/batches", otherKey.Key, create); rec.Code != 404 {
		t.Fatalf("other team's batch = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "POST", "/v1/batches", key.Key, strings.Replace(create, "/v1/chat/completions", "/v1/videos", 1)); rec.Code != 400 {
		t.Fatalf("bad endpoint = %d %s", rec.Code, rec.Body)
	}

	rec = do(e.h, "POST", "/v1/batches", key.Key, create)
	var batch struct{ ID, Status string }
	if _ = json.Unmarshal(rec.Body.Bytes(), &batch); rec.Code != 200 || batch.Status != "validating" {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/v1/batches/"+batch.ID, key.Key, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "in_progress") {
		t.Fatalf("retrieve = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/v1/batches/"+batch.ID, otherKey.Key, ""); rec.Code != 404 {
		t.Fatalf("other team's retrieve = %d", rec.Code)
	}

	// Finished: charged once, at half price, and the output file is the
	// owner's to download.
	second.set(func(u *batchUpstream) { u.status = "completed" })
	for range 2 {
		if rec := do(e.h, "GET", "/v1/batches/"+batch.ID, key.Key, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), "file_out") {
			t.Fatalf("finished = %d %s", rec.Code, rec.Body)
		}
	}
	if rec := do(e.h, "GET", "/v1/files/file_out/content", key.Key, ""); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"custom_id":"a"`) {
		t.Fatalf("output = %d %s", rec.Code, rec.Body)
	}
	if rec := do(e.h, "GET", "/v1/files/file_out/content", otherKey.Key, ""); rec.Code != 404 {
		t.Fatalf("other team's output = %d", rec.Code)
	}
	if rec := do(e.h, "GET", "/v1/batches", key.Key, ""); !strings.Contains(rec.Body.String(), `"status":"completed"`) {
		t.Fatalf("list = %s", rec.Body)
	}

	// Operators see it with its cost, and can cancel another.
	var admin struct {
		Data []store.Batch `json:"data"`
	}
	e.call("GET", "/admin/v1/batches?org_id="+org.ID, "", 200, &admin)
	if len(admin.Data) != 1 || admin.Data[0].CostUSD != 1 || admin.Data[0].Status != "completed" || admin.Data[0].ChargedAt == nil {
		t.Fatalf("admin list = %+v", admin.Data)
	}
	second.set(func(u *batchUpstream) { u.status = "in_progress" })
	rec = do(e.h, "POST", "/v1/batches", key.Key, create)
	_ = json.Unmarshal(rec.Body.Bytes(), &batch)
	var cancelled store.Batch
	e.call("POST", "/admin/v1/batches/"+batch.ID+"/cancel", "", 200, &cancelled)
	if cancelled.Status != "cancelling" {
		t.Fatalf("cancelled = %+v", cancelled)
	}
	var audit struct {
		Data []store.AuditEntry `json:"data"`
	}
	e.call("GET", "/admin/v1/audit?org_id="+org.ID, "", 200, &audit)
	if len(audit.Data) == 0 || audit.Data[0].Action != "batch.cancel" || audit.Data[0].ResourceID != batch.ID {
		t.Fatalf("audit = %+v", audit.Data)
	}

	// The refresh loop finishes and charges batches nobody polls.
	second.set(func(u *batchUpstream) { u.status = "completed" })
	defer func(d time.Duration) { batchStale = d }(batchStale)
	batchStale = -time.Second
	e.srv.pollBatches(context.Background())
	e.call("GET", "/admin/v1/batches?org_id="+org.ID+"&status=completed", "", 200, &admin)
	if len(admin.Data) != 2 {
		t.Fatalf("after refresh = %+v", admin.Data)
	}

	e.srv.Close(context.Background())
	var usage struct {
		Data []store.UsageRow `json:"data"`
	}
	e.call("GET", "/admin/v1/usage?org_id="+org.ID+"&group_by=endpoint", "", 200, &usage)
	if len(usage.Data) != 1 || usage.Data[0].Group != "batches" || usage.Data[0].Requests != 2 || usage.Data[0].CostUSD != 2 || usage.Data[0].InputTokens != 2000000 {
		t.Fatalf("usage = %+v", usage.Data)
	}
}

func TestBatchGuardrails(t *testing.T) {
	up := newBatchUpstream(t, "up")
	e := newAdminEnvConfig(t, `
store: {driver: sqlite, dsn: unused}
providers: {up: {base_url: "`+up.URL+`", api_key: key-up}}
models: {chat: {targets: [{provider: up, model: gpt}]}}
guardrails:
  no-pii: {type: pii, detect: [email], action: redact, stages: [input]}
  no-secrets: {type: pii, detect: [secret], action: block, stages: [input]}
  out: {type: pii, detect: [email], action: redact, stages: [output]}
keys:
  - {name: dev, key: `+devKey+`, guardrails: [no-pii, no-secrets]}
  - {name: guarded, key: `+guardKey+`, guardrails: [out]}
`)
	body, ct := batchFile(t, "batch", chatLine("a", "chat", "mail bob@example.com"), chatLine("b", "chat", "fine"))
	rec := doType(e.h, "/v1/files", devKey, body, ct)
	var file struct{ ID string }
	if _ = json.Unmarshal(rec.Body.Bytes(), &file); rec.Code != 200 {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	if got := up.file(file.ID); strings.Contains(got, "bob@example.com") || !strings.Contains(got, "[REDACTED:email]") {
		t.Fatalf("uploaded %s", got)
	}

	body, ct = batchFile(t, "batch", chatLine("a", "chat", "fine"), chatLine("b", "chat", "my key is AKIAABCDEFGHIJKLMNOP"))
	if rec := doType(e.h, "/v1/files", devKey, body, ct); rec.Code != 400 || errorCode(t, rec) != codeGuardrailBlocked || !strings.Contains(rec.Body.String(), "request b") {
		t.Fatalf("blocked = %d %s", rec.Code, rec.Body)
	}
	body, ct = batchFile(t, "batch", chatLine("a", "chat", "fine"))
	if rec := doType(e.h, "/v1/files", guardKey, body, ct); rec.Code != 400 || !strings.Contains(rec.Body.String(), "output guardrails") {
		t.Fatalf("output guardrails = %d %s", rec.Code, rec.Body)
	}
}

func TestBatchesWithoutDatabase(t *testing.T) {
	up := newBatchUpstream(t, "up")
	h := gateway(t, `
providers: {up: {base_url: "`+up.URL+`", api_key: key-up}}
models: {chat: {targets: [{provider: up, model: gpt}]}}
keys: [{name: dev, key: `+devKey+`}]
`, nil)
	body, ct := batchFile(t, "batch", chatLine("a", "chat", "hi"))
	if rec := doType(h, "/v1/files", devKey, body, ct); rec.Code != 501 || errorCode(t, rec) != codeNoDatabase {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body)
	}
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/files", ""},
		{"POST", "/v1/batches", `{"input_file_id":"f","endpoint":"/v1/chat/completions"}`},
		{"GET", "/v1/batches/b", ""},
	} {
		if rec := do(h, c.method, c.path, devKey, c.body); rec.Code != 501 {
			t.Fatalf("%s %s = %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
}
