package main

import (
	"context"
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
	// events are the changes, for watches.
	events []fakeEvent
	// missing are plurals whose CRD is not installed.
	missing map[string]bool
}

type fakeEvent struct {
	rv     int
	plural string
	line   []byte
}

// emit records a change for watches. The caller holds the lock.
func (f *fakeKube) emit(plural, typ string, o map[string]any) {
	line, _ := json.Marshal(map[string]any{"type": typ, "object": o})
	f.events = append(f.events, fakeEvent{f.rv, plural, append(line, '\n')})
}

// watch streams the events of a plural after a resource version.
func (f *fakeKube) watch(w http.ResponseWriter, r *http.Request, plural string) {
	after, _ := strconv.Atoi(r.URL.Query().Get("resourceVersion"))
	sent := 0
	for r.Context().Err() == nil {
		f.mu.Lock()
		evs := f.events[sent:]
		sent = len(f.events)
		f.mu.Unlock()
		for _, ev := range evs {
			if ev.plural == plural && ev.rv > after {
				_, _ = w.Write(ev.line)
			}
		}
		w.(http.Flusher).Flush()
		time.Sleep(5 * time.Millisecond)
	}
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
	f.emit(plural, "ADDED", f.objects[plural+"/"+ns+"/"+name])
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
	}
	f.emit(plural, "MODIFIED", o)
	f.collect(plural+"/"+ns+"/"+name, o)
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
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	f.mu.Lock()
	if f.missing[parts[len(parts)-1]] {
		f.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.URL.Query().Get("watch") == "true" {
		f.mu.Unlock()
		f.watch(w, r, parts[len(parts)-1])
		return
	}
	defer f.mu.Unlock()
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
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"resourceVersion": strconv.Itoa(f.rv)}, "items": items})
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
	f.emit(plural, "MODIFIED", o)
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

// syncBuffer is the operator's log, read while it runs.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// The operator applies a change when it is made, without waiting for
// the next pass.
func TestOperatorWatches(t *testing.T) {
	gw := gateway(t)
	k, kubeURL := newFakeKube(t)
	k.missing = map[string]bool{pluralSets: true}
	k.add(pluralOrgs, "ai", "acme", map[string]any{"rpm": 100.0})
	var log syncBuffer
	o := &operator{k: newKube(kubeURL, "", nil), a: &applier{c: &client{base: gw, token: token, http: &http.Client{Timeout: time.Minute}, stdout: &log}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// The interval is long, so only watches can explain later passes.
	go func() { done <- o.run(ctx, time.Hour) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	wait := func(want string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !strings.Contains(log.String(), want); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the operator never logged %q:\n%s", want, log.String())
			}
		}
	}
	wait("created org acme")
	if !strings.Contains(log.String(), "GatewayCedarPolicySet is not installed") {
		t.Fatalf("no word on the missing CRD:\n%s", log.String())
	}
	k.add(pluralOrgs, "ai", "globex", map[string]any{})
	wait("created org globex")
	k.update(pluralOrgs, "ai", "acme", map[string]any{"rpm": 50.0}, false)
	wait("updated org acme")
	k.update(pluralOrgs, "ai", "globex", nil, true)
	wait("deleted org globex")
	// The operator's own status and finalizer writes start no passes:
	// the log goes quiet, and the statuses are as the last pass left them.
	time.Sleep(600 * time.Millisecond)
	before := log.String()
	time.Sleep(600 * time.Millisecond)
	if after := log.String(); after != before {
		t.Fatalf("the operator kept working:\n%s", after[len(before):])
	}
	if s := ready(t, k.get(pluralOrgs, "ai", "acme")); !strings.HasPrefix(s, "True") {
		t.Fatalf("acme status = %s", s)
	}
}

func TestOperatorCedarPolicySets(t *testing.T) {
	gw := gateway(t)
	k, kubeURL := newFakeKube(t)
	const forbidEU = `forbid (principal, action == Action::"invoke_model", resource) when { resource.residency != "eu" };`
	const forbidTool = `forbid (principal, action == Action::"call_tool", resource);`
	k.add(pluralOrgs, "ai", "acme", map[string]any{"cedar": forbidTool})
	k.add(pluralOrgs, "ai", "globex", map[string]any{})
	k.add(pluralSets, "ai", "residency", map[string]any{"allOrganizations": true, "cedar": forbidEU})
	k.add(pluralSets, "ai", "stray", map[string]any{"organizations": []any{"acme", "nobody"}, "cedar": forbidEU})
	k.add(pluralSets, "ai", "empty", map[string]any{"allOrganizations": true})
	operate := func() string {
		t.Helper()
		return ctl(t, gw, "operator", "-once", "-kube-url", kubeURL)
	}
	out := operate()
	exported := ctl(t, gw, "config", "export")
	for _, want := range []string{"GatewayCedarPolicySet ai/residency", "GatewayCedarPolicySet ai/stray", "The organization's own policies", "call_tool"} {
		if !strings.Contains(exported, want) {
			t.Fatalf("export misses %q:\n%s\n%s", want, exported, out)
		}
	}
	if n := strings.Count(exported, "GatewayCedarPolicySet ai/residency"); n != 2 {
		t.Fatalf("the residency set is in %d organizations, want 2:\n%s", n, exported)
	}
	if strings.Contains(out, "gatewaycedarpolicysets ai/residency") {
		t.Fatalf("residency failed:\n%s", out)
	}
	if s := ready(t, k.get(pluralSets, "ai", "residency")); !strings.HasPrefix(s, "True") {
		t.Fatalf("residency status = %s", s)
	}
	if s := ready(t, k.get(pluralSets, "ai", "stray")); !strings.HasPrefix(s, "False") || !strings.Contains(s, "nobody") {
		t.Fatalf("stray status = %s", s)
	}
	if s := ready(t, k.get(pluralSets, "ai", "empty")); !strings.HasPrefix(s, "False") || !strings.Contains(s, "spec.cedar") {
		t.Fatalf("empty status = %s", s)
	}
	// A second pass changes nothing.
	if out := operate(); strings.Contains(out, "updated") {
		t.Fatalf("second pass changed things:\n%s", out)
	}
	// Policies the gateway refuses fail the set and the organizations it selects.
	k.update(pluralSets, "ai", "residency", map[string]any{"allOrganizations": true, "cedar": "permit nonsense"}, false)
	operate()
	if s := ready(t, k.get(pluralSets, "ai", "residency")); !strings.HasPrefix(s, "False") {
		t.Fatalf("broken set status = %s", s)
	}
	if s := ready(t, k.get(pluralOrgs, "ai", "globex")); !strings.HasPrefix(s, "False") {
		t.Fatalf("globex status with a broken set = %s", s)
	}
	// Deleting the sets leaves each organization its own policies.
	for _, name := range []string{"residency", "stray", "empty"} {
		k.update(pluralSets, "ai", name, nil, true)
	}
	operate()
	exported = ctl(t, gw, "config", "export")
	if strings.Contains(exported, "GatewayCedarPolicySet") || !strings.Contains(exported, "call_tool") {
		t.Fatalf("export after the sets were deleted:\n%s", exported)
	}
	if k.get(pluralSets, "ai", "residency") != nil {
		t.Fatal("a deleted set was kept")
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
