package stability

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native/nativetest"
)

func TestImages(t *testing.T) {
	n := 0
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		n++
		if n == 2 {
			_, _ = w.Write([]byte(`{"image":"Ymx1cg==","finish_reason":"CONTENT_FILTERED","seed":2}`))
			return
		}
		_, _ = w.Write([]byte(`{"image":"aQ==","finish_reason":"SUCCESS","seed":1}`))
	}}
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.ImageGenerations, Model: "sd3.5-large",
		Body: []byte(`{"prompt":"a falcon","n":3,"size":"1792x1024","seed":7}`)})
	if len(up.Calls) != 3 {
		t.Fatalf("%d calls", len(up.Calls))
	}
	c := up.Calls[0]
	if c.Path != "/v2beta/stable-image/generate/sd3" || c.Header.Get("Authorization") != "Bearer up-key" || c.Header.Get("Accept") != "application/json" {
		t.Fatalf("call %+v", c)
	}
	for _, want := range []string{"a falcon", `name="aspect_ratio"`, "16:9", `name="model"`, "sd3.5-large", `name="seed"`, "\r\n7\r\n"} {
		if !strings.Contains(c.Body, want) {
			t.Errorf("first body lacks %q", want)
		}
	}
	if !strings.Contains(up.Calls[2].Body, "\r\n9\r\n") {
		t.Error("each image gets its own seed")
	}
	if resp.StatusCode != 200 || resp.Usage.Images != 2 || strings.Count(body, `"b64_json":"aQ=="`) != 2 || strings.Contains(body, "Ymx1cg") {
		t.Fatalf("%d %s %+v", resp.StatusCode, body, resp.Usage)
	}
	if path, m := service("stable-image-ultra"); path != "/v2beta/stable-image/generate/ultra" || m != "" {
		t.Error("ultra")
	}
	if path, _ := service("stable-image-core"); path != "/v2beta/stable-image/generate/core" {
		t.Error("core")
	}
	resp, _ = nativetest.Send(t, p, &provider.Request{Endpoint: provider.ImageGenerations, Model: "m", Body: []byte(`{"prompt":"x","n":9}`)})
	if resp.StatusCode != 400 {
		t.Errorf("n=9: %d", resp.StatusCode)
	}
}

func TestError(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"id":"e","name":"content_moderation","errors":["Your request was flagged by our content moderation system."]}`))
	}}
	resp, body := nativetest.Send(t, New(up.Start(t), http.DefaultClient), &provider.Request{Endpoint: provider.ImageGenerations, Model: "stable-image-core", Body: []byte(`{"prompt":"x"}`)})
	if resp.StatusCode != 403 || !strings.Contains(body, "flagged by our content moderation") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
