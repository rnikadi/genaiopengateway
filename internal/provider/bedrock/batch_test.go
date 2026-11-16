package bedrock

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

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// batchUploadBody is a POST /files body holding a batch input file.
func batchUploadBody(t *testing.T, jsonl string) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("purpose", "batch")
	part, err := mw.CreateFormFile("file", "reqs.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, jsonl)
	_ = mw.Close()
	return &buf, mw.FormDataContentType()
}

func TestBatchOnBedrock(t *testing.T) {
	const jobID = "abc123def456"
	var mu sync.Mutex
	objects := map[string][]byte{}
	status := "InProgress"
	var job map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if key, ok := strings.CutPrefix(r.URL.Path, "/s3/acme-ai/"); ok {
			if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/s3/aws4_request") || r.Header.Get("X-Amz-Content-Sha256") == "" {
				w.WriteHeader(403)
				return
			}
			switch r.Method {
			case http.MethodPut:
				objects[key] = b
			case http.MethodDelete:
				delete(objects, key)
				w.WriteHeader(204)
			default:
				data, ok := objects[key]
				if !ok {
					w.WriteHeader(404)
					return
				}
				_, _ = w.Write(data)
			}
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request") {
			w.WriteHeader(403)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/model-invocation-job":
			_ = json.Unmarshal(b, &job)
			_, _ = w.Write([]byte(`{"jobArn":"arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/` + jobID + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/model-invocation-job/"+jobID+"/stop":
			status = "Stopping"
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/model-invocation-job/"+jobID:
			_, _ = w.Write([]byte(`{"status":"` + status + `","submitTime":"2026-11-16T10:00:00Z","totalRecordCount":3,"message":"bad role"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := config.Provider{Type: config.ProviderBedrock, BaseURL: srv.URL, Region: "us-east-1", Timeout: config.Duration(5 * time.Second),
		S3OutputURI: "s3://acme-ai/gateway", BatchRoleARN: "arn:aws:iam::123456789012:role/batch"}
	p := newWithCredentials(cfg, srv.Client(), staticCreds)
	p.control = srv.URL
	p.s3URL = func(bucket, key string) string { return srv.URL + "/s3/" + bucket + "/" + key }
	const model = "anthropic.claude-sonnet-4-5-20250929-v1:0"
	if !p.SupportsModel(provider.Batches, model) || p.SupportsModel(provider.Batches, "amazon.titan-embed-text-v2:0") {
		t.Fatal("SupportsModel")
	}
	bare := newWithCredentials(config.Provider{Type: config.ProviderBedrock, Region: "us-east-1", S3OutputURI: "s3://acme-ai/gateway"}, srv.Client(), staticCreds)
	if bare.SupportsModel(provider.Batches, model) {
		t.Fatal("batches need a role")
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

	// Lines that are not chat completions are refused.
	body, ct := batchUploadBody(t, `{"custom_id":"e","method":"POST","url":"/v1/embeddings","body":{"model":"`+model+`","input":"x"}}`)
	if code, out := call("POST", "/files", body, ct); code != 400 || !strings.Contains(out, "/v1/chat/completions") {
		t.Fatalf("embeddings upload: %d %s", code, out)
	}

	lines := []string{
		`{"custom_id":"first","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","max_tokens":50,"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi"}]}}`,
		`{"custom_id":"second","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"Bye"}]}}`,
		`{"custom_id":"third","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"Fail"}]}}`,
	}
	body, ct = batchUploadBody(t, strings.Join(lines, "\n")+"\n")
	code, out := call("POST", "/files", body, ct)
	var file struct {
		ID, Purpose, Filename string
		Bytes                 int
	}
	if err := json.Unmarshal([]byte(out), &file); err != nil || code != 200 || !batchFileID.MatchString(file.ID) || file.Purpose != "batch" || file.Filename != "reqs.jsonl" || file.Bytes == 0 {
		t.Fatalf("upload: %d %s", code, out)
	}
	fid := strings.TrimPrefix(file.ID, "file_br_")
	folder := "gateway/batches/" + fid + "/"
	var rec struct {
		RecordID   string `json:"recordId"`
		ModelInput struct {
			System   []map[string]any `json:"system"`
			Messages []map[string]any `json:"messages"`
			Config   map[string]any   `json:"inferenceConfig"`
		} `json:"modelInput"`
	}
	input := strings.Split(strings.TrimSpace(string(objects[folder+"input.jsonl"])), "\n")
	if len(input) != 3 || json.Unmarshal([]byte(input[0]), &rec) != nil || rec.RecordID != "r0000000000" ||
		len(rec.ModelInput.System) != 1 || len(rec.ModelInput.Messages) != 1 || rec.ModelInput.Config["maxTokens"] != 50.0 {
		t.Fatalf("input.jsonl: %s", objects[folder+"input.jsonl"])
	}
	if string(objects[folder+"source.jsonl"]) != strings.Join(lines, "\n")+"\n" || objects[folder+"meta.json"] == nil {
		t.Fatalf("stored objects: %v", objects)
	}
	if code, out := call("GET", "/files/"+file.ID, nil, ""); code != 200 || !strings.Contains(out, `"filename":"reqs.jsonl"`) {
		t.Fatalf("retrieve file: %d %s", code, out)
	}
	if code, out := call("GET", "/files/"+file.ID+"/content", nil, ""); code != 200 || out != strings.Join(lines, "\n")+"\n" {
		t.Fatalf("file content: %d %s", code, out)
	}

	// The batch is a Converse job on the translated file.
	code, out = call("POST", "/batches", strings.NewReader(`{"input_file_id":"`+file.ID+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`), "application/json")
	var batch struct {
		ID, Status, Endpoint string
		InputFileID          string           `json:"input_file_id"`
		OutputFileID         string           `json:"output_file_id"`
		ErrorFileID          string           `json:"error_file_id"`
		Counts               map[string]int64 `json:"request_counts"`
		Usage                map[string]any   `json:"usage"`
		Errors               *struct {
			Data []struct{ Message string }
		}
	}
	if err := json.Unmarshal([]byte(out), &batch); err != nil || code != 200 || batch.ID != "batch_br_"+jobID+"_"+fid || batch.Status != "validating" ||
		batch.InputFileID != file.ID || batch.Endpoint != "/v1/chat/completions" || batch.Counts["total"] != 3 {
		t.Fatalf("create: %d %s", code, out)
	}
	in := job["inputDataConfig"].(map[string]any)["s3InputDataConfig"].(map[string]any)
	outCfg := job["outputDataConfig"].(map[string]any)["s3OutputDataConfig"].(map[string]any)
	if job["modelId"] != model || job["modelInvocationType"] != "Converse" || job["roleArn"] != cfg.BatchRoleARN ||
		in["s3Uri"] != "s3://acme-ai/"+folder+"input.jsonl" || in["s3InputFormat"] != "JSONL" || outCfg["s3Uri"] != "s3://acme-ai/"+folder+"out/" ||
		!strings.HasPrefix(job["jobName"].(string), "gog-") {
		t.Fatalf("job: %v", job)
	}
	if code, out := call("POST", "/batches", strings.NewReader(`{"input_file_id":"file_br_000000000000000000000000"}`), "application/json"); code != 404 {
		t.Fatalf("create on a missing file: %d %s", code, out)
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
	if batch.Status != "in_progress" || batch.OutputFileID != "" || batch.Usage != nil {
		t.Fatalf("in progress: %+v", batch)
	}
	get("/batches/"+batch.ID+"/cancel", "POST")
	if batch.Status != "cancelling" {
		t.Fatalf("cancel: %+v", batch)
	}
	mu.Lock()
	status = "Failed"
	mu.Unlock()
	get("/batches/"+batch.ID, "GET")
	if batch.Status != "failed" || batch.Errors == nil || batch.Errors.Data[0].Message != "bad role" {
		t.Fatalf("failed: %+v", batch)
	}

	// Bedrock finishes: results in any order, one of them an error.
	mu.Lock()
	status = "PartiallyCompleted"
	objects[folder+"out/"+jobID+"/manifest.json.out"] = []byte(`{"totalRecordCount":3,"processedRecordCount":3,"successRecordCount":2,"errorRecordCount":1,"inputTokenCount":21,"outputTokenCount":9}`)
	objects[folder+"out/"+jobID+"/input.jsonl.out"] = []byte(strings.Join([]string{
		`{"recordId":"r0000000001","modelInput":{},"modelOutput":{"output":{"message":{"role":"assistant","content":[{"text":"Goodbye"}]}},"stopReason":"end_turn","usage":{"inputTokens":9,"outputTokens":4,"totalTokens":13}}}`,
		`{"recordId":"r0000000002","modelInput":{},"error":{"errorCode":400,"errorMessage":"bad request"}}`,
		`{"recordId":"r0000000000","modelInput":{},"modelOutput":{"output":{"message":{"role":"assistant","content":[{"text":"Hello"}]}},"stopReason":"max_tokens","usage":{"inputTokens":12,"outputTokens":5,"totalTokens":17}}}`,
	}, "\n") + "\n")
	mu.Unlock()
	get("/batches/"+batch.ID, "GET")
	if batch.Status != "completed" || batch.OutputFileID != "file_brout_"+jobID+"_"+fid || batch.ErrorFileID != "file_brerr_"+jobID+"_"+fid ||
		batch.Counts["completed"] != 2 || batch.Counts["failed"] != 1 || batch.Usage["input_tokens"] != 21.0 || batch.Usage["output_tokens"] != 9.0 {
		t.Fatalf("completed: %+v", batch)
	}

	type outLine struct {
		ID       string
		CustomID string `json:"custom_id"`
		Response *struct {
			StatusCode int `json:"status_code"`
			Body       struct {
				Model   string
				Choices []struct {
					Message      struct{ Content string }
					FinishReason string `json:"finish_reason"`
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
		results[0].Response.StatusCode != 200 || results[0].Response.Body.Choices[0].Message.Content != "Goodbye" || results[0].Response.Body.Model != model ||
		results[1].Response.Body.Choices[0].FinishReason != "length" || results[1].Response.Body.Usage["prompt_tokens"] != 12.0 {
		t.Fatalf("output file: %+v", results)
	}
	failed := read(batch.ErrorFileID)
	if len(failed) != 1 || failed[0].CustomID != "third" || failed[0].Response != nil || failed[0].Error.Code != "400" || failed[0].Error.Message != "bad request" {
		t.Fatalf("error file: %+v", failed)
	}

	// Ids that name nothing of this provider's reach no object.
	for _, path := range []string{"/files/file_br_..%2F..%2Fsecret/content", "/files/file-abc/content", "/batches/batch_abc", "/batches/batch_br_" + jobID + "_../x"} {
		if code, out := call("GET", path, nil, ""); code != 404 {
			t.Fatalf("%s: %d %s", path, code, out)
		}
	}

	// Deleting the input file removes what the gateway wrote; results stay.
	if code, out := call("DELETE", "/files/"+file.ID, nil, ""); code != 200 || !strings.Contains(out, `"deleted":true`) {
		t.Fatalf("delete: %d %s", code, out)
	}
	if objects[folder+"source.jsonl"] != nil || objects[folder+"input.jsonl"] != nil || objects[folder+"meta.json"] != nil || objects[folder+"out/"+jobID+"/input.jsonl.out"] == nil {
		t.Fatalf("after delete: %v", objects)
	}
	if code, _ := call("GET", "/files/"+file.ID, nil, ""); code != 404 {
		t.Fatalf("deleted file: %d", code)
	}
	if code, out := call("DELETE", "/files/"+batch.OutputFileID, nil, ""); code != 200 {
		t.Fatalf("delete output: %d %s", code, out)
	}
}
