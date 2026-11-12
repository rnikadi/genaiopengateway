package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// fakeKube serves the parts of the Kubernetes API the operator uses:
// lists of custom resources, Secrets, and merge patches, with
// resourceVersion checks and finalizer semantics.
type fakeKube struct {
	mu      sync.Mutex
	objects map[string]map[string]any // plural/ns/name
	secrets map[string]map[string]string
	rv      int
}

func newFakeKube(t *testing.T) (*fakeKube, string) {
	f := &fakeKube{objects: map[string]map[string]any{}, secrets: map[string]map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

func (f *fakeKube) add(plural, ns, name string, spec map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rv++
	f.objects[plural+"/"+ns+"/"+name] = map[string]any{
		"metadata": map[string]any{"name": name, "namespace": ns, "generation": 1.0, "resourceVersion": strconv.Itoa(f.rv)},
		"spec":     spec,
	}
}

func (f *fakeKube) get(plural, ns, name string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[plural+"/"+ns+"/"+name]
	if o == nil {
		return nil
	}
	b, _ := json.Marshal(o)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// update changes a resource as kubectl would: a new spec bumps the
// generation, a deletion sets the deletion timestamp.
func (f *fakeKube) update(plural, ns, name string, spec map[string]any, deleting bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := f.objects[plural+"/"+ns+"/"+name]
	md := o["metadata"].(map[string]any)
	f.rv++
	md["resourceVersion"] = strconv.Itoa(f.rv)
	if spec != nil {
		o["spec"] = spec
		md["generation"] = md["generation"].(float64) + 1
	}
	if deleting {
		md["deletionTimestamp"] = time.Now().UTC().Format(time.RFC3339)
		f.collect(plural+"/"+ns+"/"+name, o)
	}
}

// collect removes a deleting resource that has no finalizers left.
func (f *fakeKube) collect(key string, o map[string]any) {
	md := o["metadata"].(map[string]any)
	fins, _ := md["finalizers"].([]any)
	if md["deletionTimestamp"] != nil && len(fins) == 0 {
		delete(f.objects, key)
	}
}

func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			dm, _ := dst[k].(map[string]any)
			if dm == nil {
				dm = map[string]any{}
				dst[k] = dm
			}
			merge(dm, sm)
			continue
		}
		dst[k] = v
	}
}

func (f *fakeKube) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 6 && parts[0] == "api" && parts[4] == "secrets" {
		data, ok := f.secrets[parts[3]+"/"+parts[5]]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		return
	}
	// /apis/group/version/[namespaces/ns/]plural[/name[/status]]
	rest := parts[3:]
	ns := ""
	if len(rest) > 1 && rest[0] == "namespaces" {
		ns, rest = rest[1], rest[2:]
	}
	plural := rest[0]
	if len(rest) == 1 && r.Method == http.MethodGet {
		var items []any
		for key, o := range f.objects {
			if strings.HasPrefix(key, plural+"/") && (ns == "" || strings.HasPrefix(key, plural+"/"+ns+"/")) {
				items = append(items, o)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		return
	}
	key := plural + "/" + ns + "/" + rest[1]
	o := f.objects[key]
	if o == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPatch || r.Header.Get("Content-Type") != "application/merge-patch+json" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var patch map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &patch)
	md := o["metadata"].(map[string]any)
	if pm, ok := patch["metadata"].(map[string]any); ok {
		if rv, ok := pm["resourceVersion"]; ok && rv != md["resourceVersion"] {
			w.WriteHeader(http.StatusConflict)
			return
		}
	}
	if len(rest) == 3 && rest[2] == "status" {
		patch = map[string]any{"status": patch["status"]}
	} else {
		delete(patch, "status")
	}
	merge(o, patch)
	f.rv++
	md["resourceVersion"] = strconv.Itoa(f.rv)
	f.collect(key, o)
	_ = json.NewEncoder(w).Encode(o)
}

func ready(t *testing.T, o map[string]any) string {
	t.Helper()
	st, _ := o["status"].(map[string]any)
	conds, _ := st["conditions"].([]any)
	if len(conds) == 0 {
		return "no status"
	}
	c := conds[0].(map[string]any)
	return c["status"].(string) + ": " + c["message"].(string)
}

func TestOperator(t *testing.T) {
	gw := gateway(t)
	k, kubeURL := newFakeKube(t)
	k.secrets["ai/openai"] = map[string]string{"api_key": "c2stZnJvbS1hLXNlY3JldA=="} // sk-from-a-secret
	k.add("gatewayconfigobjects", "ai", "openai", map[string]any{
		"kind": "providers", "spec": map[string]any{"type": "openai", "base_url": "http://127.0.0.1:9/v1"},
		"secretRefs": []any{map[string]any{"path": "api_key", "secretKeyRef": map[string]any{"name": "openai", "key": "api_key"}}},
	})
	k.add("gatewayconfigobjects", "ai", "chat", map[string]any{
		"kind": "models", "spec": map[string]any{"targets": []any{map[string]any{"provider": "openai", "model": "gpt-5-mini"}}},
	})
	k.add("gatewayconfigobjects", "ai", "bad", map[string]any{"kind": "bogus", "spec": map[string]any{}})
	k.add("gatewayorganizations", "ai", "acme", map[string]any{
		"allowed_models": []any{"chat"}, "rpm": 100.0, "teams": map[string]any{"search": map[string]any{"rpm": 10.0}},
	})

	operate := func() string {
		t.Helper()
		return ctl(t, gw, "operator", "-once", "-kube-url", kubeURL)
	}
	out := operate()
	for _, want := range []string{"put providers/openai", "put models/chat", "created org acme", "created team acme/search"} {
		if !strings.Contains(out, want) {
			t.Fatalf("first pass misses %q:\n%s", want, out)
		}
	}
	if s := ready(t, k.get("gatewayconfigobjects", "ai", "bad")); !strings.Contains(s, "False") || !strings.Contains(s, "bogus") {
		t.Fatalf("bad object status = %s", s)
	}
	for _, name := range []string{"openai", "chat"} {
		o := k.get("gatewayconfigobjects", "ai", name)
		if s := ready(t, o); !strings.HasPrefix(s, "True") {
			t.Fatalf("%s status = %s", name, s)
		}
		if fins := o["metadata"].(map[string]any)["finalizers"].([]any); len(fins) != 1 || fins[0] != finalizer {
			t.Fatalf("%s finalizers = %v", name, fins)
		}
	}
	exported := ctl(t, gw, "config", "export")
	if strings.Contains(exported, "sk-from-a-secret") || !strings.Contains(exported, "api_key: set") || !strings.Contains(exported, "search") {
		t.Fatalf("export:\n%s", exported)
	}

	// A second pass changes nothing.
	if out := operate(); strings.Contains(out, "put ") || strings.Contains(out, "created") || strings.Contains(out, "updated") {
		t.Fatalf("second pass changed things:\n%s", out)
	}

	// A change in the UI is reverted; a new spec applies.
	ctl(t, gw, "config", "import", writeTemp(t, `objects: {models: {chat: {targets: [{provider: openai, model: drifted}]}}}`))
	k.update("gatewayorganizations", "ai", "acme", map[string]any{"rpm": 50.0, "teams": map[string]any{"ml": map[string]any{}}}, false)
	out = operate()
	for _, want := range []string{"put models/chat", "updated org acme", "created team acme/ml", "deleted team acme/search"} {
		if !strings.Contains(out, want) {
			t.Fatalf("third pass misses %q:\n%s", want, out)
		}
	}

	// Deleting resources removes what they made, then lets them go.
	k.update("gatewayorganizations", "ai", "acme", nil, true)
	k.update("gatewayconfigobjects", "ai", "chat", nil, true)
	out = operate()
	for _, want := range []string{"deleted org acme", "deleted models/chat"} {
		if !strings.Contains(out, want) {
			t.Fatalf("deletion pass misses %q:\n%s", want, out)
		}
	}
	if k.get("gatewayorganizations", "ai", "acme") != nil || k.get("gatewayconfigobjects", "ai", "chat") != nil {
		t.Fatal("deleted resources were kept")
	}
	if strings.Contains(ctl(t, gw, "orgs", "list"), "acme") {
		t.Fatal("the organization is still in the gateway")
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := createTemp(t)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return f.Name()
}

func createTemp(t *testing.T) (*os.File, error) {
	return os.CreateTemp(t.TempDir(), "*.yaml")
}

// Every stored kind can be imported and managed by the operator.
func TestKindOrderCoversStoredKinds(t *testing.T) {
	for _, k := range config.Kinds {
		if !slices.Contains(kindOrder, k) {
			t.Errorf("kindOrder misses %s", k)
		}
	}
}
