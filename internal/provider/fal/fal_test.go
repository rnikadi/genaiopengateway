package fal

import (
	"net/http"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/provider"
	"github.com/rnikadi/genaiopengateway/internal/provider/native/nativetest"
)

func TestImages(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		_, _ = w.Write([]byte(`{"images":[{"url":"https://v3.fal.media/a.png","width":1024,"height":768},{"url":"https://v3.fal.media/b.png"}],"has_nsfw_concepts":[false,true],"seed":1}`))
	}}
	p := New(up.Start(t), http.DefaultClient)
	resp, body := nativetest.Send(t, p, &provider.Request{Endpoint: provider.ImageGenerations, Model: "fal-ai/flux/dev",
		Body: []byte(`{"prompt":"a falcon","n":2,"size":"1024x768"}`)})
	c := up.Calls[0]
	if c.Path != "/fal-ai/flux/dev" || c.Header.Get("Authorization") != "Key up-key" || c.Body != `{"image_size":{"height":768,"width":1024},"num_images":2,"prompt":"a falcon"}` {
		t.Fatalf("call %+v", c)
	}
	if resp.StatusCode != 200 || resp.Usage.Images != 1 || !strings.Contains(body, `"url":"https://v3.fal.media/a.png"`) || strings.Contains(body, "b.png") {
		t.Fatalf("%d %s %+v", resp.StatusCode, body, resp.Usage)
	}
	for _, m := range []string{"../admin", "fal-ai/x?y=1"} {
		if resp, _ := nativetest.Send(t, p, &provider.Request{Endpoint: provider.ImageGenerations, Model: m, Body: []byte(`{"prompt":"x"}`)}); resp.StatusCode != 400 {
			t.Errorf("model %q: %d", m, resp.StatusCode)
		}
	}
}

func TestError(t *testing.T) {
	up := &nativetest.Upstream{Handler: func(w http.ResponseWriter, r *http.Request, _ string) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"detail":[{"loc":["body","prompt"],"msg":"field required","type":"value_error.missing"}]}`))
	}}
	resp, body := nativetest.Send(t, New(up.Start(t), http.DefaultClient), &provider.Request{Endpoint: provider.ImageGenerations, Model: "fal-ai/flux/dev", Body: []byte(`{"prompt":"x"}`)})
	if resp.StatusCode != 422 || !strings.Contains(body, "field required") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}
