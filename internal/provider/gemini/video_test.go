package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/provider"
)

func TestVeo(t *testing.T) {
	const opName = "models/veo-3.0-generate-001/operations/abc123"
	polls := 0
	var srvURL string
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls = append(calls, r.Method+" "+r.URL.Path+" "+string(b))
		if r.Header.Get("X-Goog-Api-Key") != "goog-key" {
			w.WriteHeader(401)
			return
		}
		switch r.URL.Path {
		case "/v1beta/models/veo-3.0-generate-001:predictLongRunning":
			_, _ = w.Write([]byte(`{"name":"` + opName + `"}`))
		case "/v1beta/" + opName:
			polls++
			if polls == 1 {
				_, _ = w.Write([]byte(`{"name":"` + opName + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"name":"` + opName + `","done":true,"response":{"generateVideoResponse":{"generatedSamples":[{"video":{"uri":"` + srvURL + `/v1beta/files/f1:download?alt=media"}}]}}}`))
		case "/v1beta/files/f1:download":
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("mp4data"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	srvURL = srv.URL
	p := New(config.Provider{BaseURL: srv.URL, APIKey: "goog-key", Timeout: config.Duration(5 * time.Second)}, srv.Client())
	if !p.SupportsModel(provider.Videos, "veo-3.0-generate-001") || p.SupportsModel(provider.ChatCompletions, "veo-3.0-generate-001") {
		t.Fatal("SupportsModel")
	}
	resp, err := p.Send(context.Background(), &provider.Request{Endpoint: provider.Videos, Model: "veo-3.0-generate-001",
		Body: []byte(`{"model":"v","prompt":"a falcon","seconds":"4","size":"1080x1920"}`)})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	var job struct{ ID, Status, Seconds string }
	if err := json.Unmarshal(b, &job); err != nil || job.Status != "queued" || job.Seconds != "4" || resp.Usage.VideoSeconds != 4 || !strings.HasPrefix(job.ID, videoPrefix) {
		t.Fatalf("%s %+v", b, resp.Usage)
	}
	if want := `{"instances":[{"prompt":"a falcon"}],"parameters":{"aspectRatio":"9:16","durationSeconds":4,"resolution":"1080p"}}`; !strings.HasSuffix(calls[0], want) {
		t.Fatalf("create %s", calls[0])
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
	if code, b := get("/videos/" + job.ID); code != 200 || !strings.Contains(b, `"status":"in_progress"`) {
		t.Fatalf("first poll %d %s", code, b)
	}
	if code, b := get("/videos/" + job.ID); code != 200 || !strings.Contains(b, `"status":"completed"`) || !strings.Contains(b, `"progress":100`) {
		t.Fatalf("second poll %d %s", code, b)
	}
	if code, b := get("/videos/" + job.ID + "/content"); code != 200 || b != "mp4data" {
		t.Fatalf("content %d %s", code, b)
	}
	if code, _ := get("/videos/veo_" + "bm90LWFuLW9w"); code != 404 {
		t.Errorf("an id naming no operation = %d", code)
	}
	r, _ := p.Forward(context.Background(), http.MethodDelete, "/videos/"+job.ID, nil, "")
	if r.StatusCode != 200 {
		t.Errorf("delete %d", r.StatusCode)
	}
}

func TestVeoForeignDownload(t *testing.T) {
	p := New(config.Provider{BaseURL: "https://generativelanguage.googleapis.com", APIKey: "k", Timeout: config.Duration(time.Second)}, http.DefaultClient)
	r, err := p.download(context.Background(), "https://evil.example/f")
	if err != nil || r.StatusCode != http.StatusBadGateway {
		t.Fatalf("%v %v", r, err)
	}
}
