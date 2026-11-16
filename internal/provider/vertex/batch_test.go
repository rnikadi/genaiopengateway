package vertex

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func TestBatchOnVertex(t *testing.T) {
	const jobs = "/v1/projects/acme-ai/locations/europe-west4/batchPredictionJobs"
	var mu sync.Mutex
	objects := map[string][]byte{}
	state, outputDir := "JOB_STATE_RUNNING", ""
	var job map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		name, isObject := strings.CutPrefix(r.URL.Path, "/storage/v1/b/acme-ai/o/")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/upload/storage/v1/b/acme-ai/o" && r.URL.Query().Get("uploadType") == "media":
			objects[r.URL.Query().Get("name")] = b
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/storage/v1/b/acme-ai/o":
			var items []map[string]string
			for n := range objects {
				if strings.HasPrefix(n, r.URL.Query().Get("prefix")) {
					items = append(items, map[string]string{"name": n})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		case isObject && r.Method == http.MethodDelete:
			delete(objects, name)
			w.WriteHeader(204)
		case isObject && r.URL.Query().Get("alt") == "media":
			data, ok := objects[name]
			if !ok {
				w.WriteHeader(404)
				return
			}
			_, _ = w.Write(data)
		case r.Method == http.MethodPost && r.URL.Path == jobs:
			_ = json.Unmarshal(b, &job)
			_, _ = w.Write([]byte(`{"name":"projects/1234/locations/europe-west4/batchPredictionJobs/777","state":"JOB_STATE_PENDING"}`))
		case r.Method == http.MethodPost && r.URL.Path == jobs+"/777:cancel":
			state = "JOB_STATE_CANCELLING"
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == jobs+"/777":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"state": state, "createTime": "2026-11-16T10:00:00Z", "error": map[string]string{"message": "quota"},
				"outputInfo": map[string]string{"gcsOutputDirectory": outputDir},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := config.Provider{Type: config.ProviderVertex, BaseURL: srv.URL, Project: "acme-ai", Location: "europe-west4",
		Timeout: config.Duration(5 * time.Second), GCSOutputURI: "gs://acme-ai/gateway"}
	p := newWithTokens(cfg, srv.Client(), &tokens{ts: oauth2.ReuseTokenSource(nil, &fakeTokens{})})
	p.gcsBase = srv.URL
	if !provider.SupportsModel(p, provider.Batches, "gemini-2.5-flash") || provider.SupportsModel(p, provider.Batches, "anthropic/claude-sonnet-4-5") ||
		provider.SupportsModel(p, provider.Batches, "veo-3.0-generate-001") {
		t.Fatal("SupportsModel")
	}
	bare := cfg
	bare.GCSOutputURI = ""
	if provider.SupportsModel(newWithTokens(bare, srv.Client(), &tokens{}), provider.Batches, "gemini-2.5-flash") {
		t.Fatal("batches need a folder")
	}

	call := func(method, path string, body io.Reader, contentType string) (int, string) {
		t.Helper()
		resp, err := p.Forward(context.Background(), method, path, body, contentType)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	upload := func(jsonl string) (int, string) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("purpose", "batch")
		part, _ := mw.CreateFormFile("file", "reqs.jsonl")
		_, _ = io.WriteString(part, jsonl)
		_ = mw.Close()
		return call("POST", "/files", &buf, mw.FormDataContentType())
	}

	if code, out := upload(`{"custom_id":"c","method":"POST","url":"/v1/chat/completions","body":{"model":"anthropic/claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}}`); code != 400 || !strings.Contains(out, "Gemini") {
		t.Fatalf("claude upload: %d %s", code, out)
	}
	lines := []string{
		`{"custom_id":"first","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","max_tokens":50,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}]}}`,
		`{"custom_id":"second","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Bye"}]}}`,
		`{"custom_id":"third","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Fail"}]}}`,
		`{"custom_id":"fourth","method":"POST","url":"/v1/chat/completions","body":{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"Blocked"}]}}`,
	}
	code, out := upload(strings.Join(lines, "\n") + "\n")
	var file struct{ ID, Purpose, Filename string }
	if err := json.Unmarshal([]byte(out), &file); err != nil || code != 200 || !batchFileID.MatchString(file.ID) || file.Purpose != "batch" || file.Filename != "reqs.jsonl" {
		t.Fatalf("upload: %d %s", code, out)
	}
	fid := strings.TrimPrefix(file.ID, "file_vx_")
	folder := "gateway/batches/" + fid + "/"
	var rec struct {
		Request struct {
			Contents          []map[string]any  `json:"contents"`
			SystemInstruction map[string]any    `json:"systemInstruction"`
			GenerationConfig  map[string]any    `json:"generationConfig"`
			Labels            map[string]string `json:"labels"`
		} `json:"request"`
	}
	input := strings.Split(strings.TrimSpace(string(objects[folder+"input.jsonl"])), "\n")
	if len(input) != 4 || json.Unmarshal([]byte(input[0]), &rec) != nil || rec.Request.Labels[rowLabel] != "r0000000000" ||
		len(rec.Request.Contents) != 1 || rec.Request.SystemInstruction == nil || rec.Request.GenerationConfig["maxOutputTokens"] != 50.0 {
		t.Fatalf("input.jsonl: %s", objects[folder+"input.jsonl"])
	}
	if code, out := call("GET", "/files/"+file.ID+"/content", nil, ""); code != 200 || out != strings.Join(lines, "\n")+"\n" {
		t.Fatalf("file content: %d %s", code, out)
	}

	code, out = call("POST", "/batches", strings.NewReader(`{"input_file_id":"`+file.ID+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`), "application/json")
	var batch struct {
		ID, Status   string
		Created      int64            `json:"created_at"`
		OutputFileID string           `json:"output_file_id"`
		ErrorFileID  string           `json:"error_file_id"`
		Counts       map[string]int64 `json:"request_counts"`
		Usage        map[string]any   `json:"usage"`
		Errors       *struct {
			Data []struct{ Message string }
		}
	}
	if err := json.Unmarshal([]byte(out), &batch); err != nil || code != 200 || batch.ID != "batch_vx_777_"+fid || batch.Status != "validating" || batch.Counts["total"] != 4 {
		t.Fatalf("create: %d %s", code, out)
	}
	src := job["inputConfig"].(map[string]any)
	dst := job["outputConfig"].(map[string]any)
	if job["model"] != "publishers/google/models/gemini-2.5-flash" || src["instancesFormat"] != "jsonl" ||
		src["gcsSource"].(map[string]any)["uris"].([]any)[0] != "gs://acme-ai/"+folder+"input.jsonl" ||
		dst["predictionsFormat"] != "jsonl" || dst["gcsDestination"].(map[string]any)["outputUriPrefix"] != "gs://acme-ai/"+folder+"out/" {
		t.Fatalf("job: %v", job)
	}

	get := func(path, method string) {
		t.Helper()
		batch.OutputFileID, batch.ErrorFileID, batch.Usage, batch.Errors = "", "", nil, nil
		code, out := call(method, path, nil, "")
		if err := json.Unmarshal([]byte(out), &batch); err != nil || code != 200 {
			t.Fatalf("%s %s: %d %s", method, path, code, out)
		}
	}
	get("/batches/"+batch.ID, "GET")
	if batch.Status != "in_progress" || batch.OutputFileID != "" || batch.Usage != nil || batch.Created != 1794823200 {
		t.Fatalf("in progress: %+v", batch)
	}
	get("/batches/"+batch.ID+"/cancel", "POST")
	if batch.Status != "cancelling" {
		t.Fatalf("cancel: %+v", batch)
	}
	mu.Lock()
	state = "JOB_STATE_FAILED"
	mu.Unlock()
	get("/batches/"+batch.ID, "GET")
	if batch.Status != "failed" || batch.Errors == nil || batch.Errors.Data[0].Message != "quota" {
		t.Fatalf("failed: %+v", batch)
	}

	// A job that names results outside the file's folder gets none read.
	mu.Lock()
	state, outputDir = "JOB_STATE_SUCCEEDED", "gs://other-bucket/secrets"
	objects["secrets/predictions.jsonl"] = []byte(`{"status":"","response":{}}` + "\n")
	mu.Unlock()
	get("/batches/"+batch.ID, "GET")
	if batch.Status != "completed" || batch.OutputFileID != "" || batch.Usage != nil {
		t.Fatalf("results elsewhere: %+v", batch)
	}

	// Vertex AI finishes: results in any order, echoing each request.
	dir := folder + "out/prediction-model-2026-11-16T10:05:00.000Z"
	answer := func(row, text string, in, outTokens int) string {
		b, _ := json.Marshal(map[string]any{
			"status": "", "processed_time": "2026-11-16T10:05:00Z",
			"request": map[string]any{"contents": []any{}, "labels": map[string]string{rowLabel: row}},
			"response": map[string]any{
				"candidates":    []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": text}}}, "finishReason": "STOP"}},
				"usageMetadata": map[string]int{"promptTokenCount": in, "candidatesTokenCount": outTokens},
				"modelVersion":  "gemini-2.5-flash",
			},
		})
		return string(b)
	}
	mu.Lock()
	outputDir = "gs://acme-ai/" + dir
	objects[dir+"/predictions.jsonl"] = []byte(strings.Join([]string{
		answer("r0000000001", "Goodbye", 9, 4),
		`{"status":"Bad Request: {\"error\": {\"code\": 400}}","request":{"labels":{"gog_row":"r0000000002"}},"response":{}}`,
		answer("r0000000000", "Hello", 12, 5),
		`{"status":"","request":{"labels":{"gog_row":"r0000000003"}},"response":{"promptFeedback":{"blockReason":"SAFETY"},"usageMetadata":{"promptTokenCount":3}}}`,
	}, "\n") + "\n")
	objects[dir+"/notes.txt"] = []byte("not results")
	mu.Unlock()
	get("/batches/"+batch.ID, "GET")
	if batch.Status != "completed" || batch.OutputFileID != "file_vxout_777_"+fid || batch.ErrorFileID != "file_vxerr_777_"+fid ||
		batch.Counts["completed"] != 2 || batch.Counts["failed"] != 2 || batch.Counts["total"] != 4 ||
		batch.Usage["input_tokens"] != 24.0 || batch.Usage["output_tokens"] != 9.0 {
		t.Fatalf("completed: %+v", batch)
	}

	type outLine struct {
		CustomID string `json:"custom_id"`
		Response *struct {
			StatusCode int `json:"status_code"`
			Body       struct {
				Model   string
				Choices []struct {
					Message struct{ Content string }
				}
				Usage map[string]any
			}
		}
		Error *struct{ Code, Message string }
	}
	read := func(id string) []outLine {
		t.Helper()
		code, out := call("GET", "/files/"+id+"/content", nil, "")
		if code != 200 {
			t.Fatalf("content of %s: %d %s", id, code, out)
		}
		var ls []outLine
		for _, raw := range strings.Split(strings.TrimSpace(out), "\n") {
			var l outLine
			if err := json.Unmarshal([]byte(raw), &l); err != nil {
				t.Fatalf("line %q: %v", raw, err)
			}
			ls = append(ls, l)
		}
		return ls
	}
	results := read(batch.OutputFileID)
	if len(results) != 2 || results[0].CustomID != "second" || results[1].CustomID != "first" || results[0].Error != nil ||
		results[0].Response.StatusCode != 200 || results[0].Response.Body.Choices[0].Message.Content != "Goodbye" ||
		results[1].Response.Body.Model != "gemini-2.5-flash" || results[1].Response.Body.Usage["prompt_tokens"] != 12.0 {
		t.Fatalf("output file: %+v", results)
	}
	failed := read(batch.ErrorFileID)
	if len(failed) != 2 || failed[0].CustomID != "third" || failed[0].Response != nil || !strings.Contains(failed[0].Error.Message, "Bad Request") ||
		failed[1].CustomID != "fourth" || !strings.Contains(failed[1].Error.Message, "SAFETY") {
		t.Fatalf("error file: %+v", failed)
	}

	for _, path := range []string{"/files/file_vx_..%2F..%2Fsecret/content", "/files/file-abc/content", "/batches/batch_abc", "/batches/batch_vx_777%2F..%2F1_" + fid} {
		if code, out := call("GET", path, nil, ""); code != 404 {
			t.Fatalf("%s: %d %s", path, code, out)
		}
	}

	if code, out := call("DELETE", "/files/"+file.ID, nil, ""); code != 200 || !strings.Contains(out, `"deleted":true`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	if objects[folder+"source.jsonl"] != nil || objects[folder+"meta.json"] != nil || objects[dir+"/predictions.jsonl"] == nil {
		t.Fatalf("after delete: %v", objects)
	}
	if code, _ := call("GET", "/files/"+file.ID, nil, ""); code != 404 {
		t.Fatalf("deleted file: %d", code)
	}
}
