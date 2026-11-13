package bedrock

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func TestNovaReel(t *testing.T) {
	const arn = "arn:aws:bedrock:us-east-1:123456789012:async-invoke/inv42"
	polls := 0
	var calls []string
	var startBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		switch {
		case r.URL.Path == "/async-invoke" && r.Method == "POST":
			_ = json.Unmarshal(b, &startBody)
			_, _ = w.Write([]byte(`{"invocationArn":"` + arn + `"}`))
		case r.URL.Path == "/async-invoke/"+arn:
			polls++
			if polls == 1 {
				_, _ = w.Write([]byte(`{"status":"InProgress"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"Completed","outputDataConfig":{"s3OutputDataConfig":{"s3Uri":"s3://videos-bucket/reel"}}}`))
		case r.URL.Path == "/s3/videos-bucket/reel/inv42/output.mp4":
			if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/s3/aws4_request") || r.Header.Get("X-Amz-Content-Sha256") == "" {
				w.WriteHeader(403)
				return
			}
			_, _ = w.Write([]byte("reel-mp4"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	cfg := config.Provider{Type: config.ProviderBedrock, BaseURL: srv.URL, Region: "us-east-1", Timeout: config.Duration(5 * time.Second), S3OutputURI: "s3://videos-bucket/reel"}
	p := newWithCredentials(cfg, srv.Client(), staticCreds)
	p.s3URL = func(bucket, key string) string { return srv.URL + "/s3/" + bucket + "/" + key }
	if !p.SupportsModel(provider.Videos, "amazon.nova-reel-v1:1") || p.SupportsModel(provider.ChatCompletions, "amazon.nova-reel-v1:1") {
		t.Fatal("SupportsModel")
	}

	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: provider.Videos, Model: "amazon.nova-reel-v1:1", Body: []byte(`{"prompt":"a falcon","seconds":"12"}`)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	var job struct{ ID, Status, Seconds string }
	if json.Unmarshal(b, &job) != nil || job.Status != "queued" || job.Seconds != "12" || resp.Usage.VideoSeconds != 12 || arnOf(job.ID) != arn {
		t.Fatalf("%s %+v", b, resp.Usage)
	}
	in := startBody["modelInput"].(map[string]any)
	if in["taskType"] != "MULTI_SHOT_AUTOMATED" || startBody["outputDataConfig"].(map[string]any)["s3OutputDataConfig"].(map[string]any)["s3Uri"] != "s3://videos-bucket/reel" {
		t.Fatalf("start %v", startBody)
	}
	get := func(path string) (int, string) {
		r, err := p.Forward(context.Background(), http.MethodGet, path, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, string(b)
	}
	if code, b := get("/videos/" + url.PathEscape(job.ID)); code != 200 || !strings.Contains(b, `"in_progress"`) {
		t.Fatalf("poll 1: %d %s", code, b)
	}
	if code, b := get("/videos/" + job.ID); code != 200 || !strings.Contains(b, `"completed"`) {
		t.Fatalf("poll 2: %d %s", code, b)
	}
	if code, b := get("/videos/" + job.ID + "/content"); code != 200 || b != "reel-mp4" {
		t.Fatalf("content %d %s (%v)", code, b, calls)
	}
	for _, bad := range []string{`{"prompt":"x","seconds":"7"}`, `{"prompt":"x","size":"720x1280"}`} {
		r, _ := p.Send(context.Background(), &provider.Request{Endpoint: provider.Videos, Model: "amazon.nova-reel-v1:1", Body: []byte(bad)})
		if r.StatusCode != 400 {
			t.Errorf("%s = %d", bad, r.StatusCode)
		}
	}
	cfg.S3OutputURI = ""
	if newWithCredentials(cfg, srv.Client(), staticCreds).SupportsModel(provider.Videos, "amazon.nova-reel-v1:1") {
		t.Error("Nova Reel without an S3 output")
	}
}
