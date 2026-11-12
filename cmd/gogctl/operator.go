package main

// The operator reconciles GatewayOrganization and GatewayConfigObject
// resources into the gateway through the admin API (docs/operator.md),
// as config import does for a file.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

const (
	crdGroup   = "genaiopengateway.io"
	crdVersion = "v1"
	// finalizer holds a resource until the operator has removed what it
	// made in the gateway.
	finalizer = crdGroup + "/cleanup"
	saDir     = "/var/run/secrets/kubernetes.io/serviceaccount"
)

var errKubeNotFound = errors.New("not found")

// kube is a minimal Kubernetes API client: the operator needs only
// lists, gets and merge patches.
type kube struct {
	base string
	// tokenFile is read for every request, since bound tokens rotate.
	tokenFile string
	http      *http.Client
}

// inCluster connects with the pod's service account.
func inCluster() (*kube, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster; pass -kube-url, such as the address of kubectl proxy")
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the service account CA does not parse")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return &kube{base: "https://" + host + ":" + port, tokenFile: saDir + "/token", http: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

func (k *kube) do(ctx context.Context, method, path, ctype string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	if k.tokenFile != "" {
		tok, err := os.ReadFile(k.tokenFile)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(tok)))
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := k.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errKubeNotFound
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %.300s", method, path, resp.Status, data)
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// resource is a custom resource as the operator reads it.
type resource struct {
	Metadata struct {
		Name              string   `json:"name"`
		Namespace         string   `json:"namespace"`
		Generation        int64    `json:"generation"`
		ResourceVersion   string   `json:"resourceVersion"`
		DeletionTimestamp *string  `json:"deletionTimestamp"`
		Finalizers        []string `json:"finalizers"`
	} `json:"metadata"`
	Spec   map[string]any `json:"spec"`
	Status struct {
		ObservedGeneration int64       `json:"observedGeneration"`
		AppliedHash        string      `json:"appliedHash"`
		Conditions         []condition `json:"conditions"`
	} `json:"status"`
}

type condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

func (r *resource) id() string { return r.Metadata.Namespace + "/" + r.Metadata.Name }

// operator reconciles resources into one gateway.
type operator struct {
	k         *kube
	a         *applier
	namespace string
}

func (o *operator) path(plural, ns, name string) string {
	p := "/apis/" + crdGroup + "/" + crdVersion
	if ns != "" {
		p += "/namespaces/" + url.PathEscape(ns)
	}
	p += "/" + plural
	if name != "" {
		p += "/" + url.PathEscape(name)
	}
	return p
}

func (o *operator) list(ctx context.Context, plural string) ([]*resource, error) {
	var out struct {
		Items []*resource `json:"items"`
	}
	if err := o.k.do(ctx, http.MethodGet, o.path(plural, o.namespace, ""), "", nil, &out); err != nil {
		return nil, err
	}
	slices.SortFunc(out.Items, func(a, b *resource) int { return strings.Compare(a.id(), b.id()) })
	return out.Items, nil
}

// setFinalizers replaces a resource's finalizers, failing if it changed
// since it was read.
func (o *operator) setFinalizers(ctx context.Context, plural string, r *resource, fins []string) error {
	patch := map[string]any{"metadata": map[string]any{"finalizers": fins, "resourceVersion": r.Metadata.ResourceVersion}}
	return o.k.do(ctx, http.MethodPatch, o.path(plural, r.Metadata.Namespace, r.Metadata.Name), "application/merge-patch+json", patch, nil)
}

// setStatus records the outcome of a reconcile when it changed.
func (o *operator) setStatus(ctx context.Context, plural string, r *resource, hash string, err error) {
	c := condition{Type: "Ready", Status: "True", Reason: "Applied", Message: "applied to the gateway"}
	if err != nil {
		c = condition{Type: "Ready", Status: "False", Reason: "Error", Message: err.Error()}
		fmt.Fprintf(o.a.c.stdout, "%s %s: %v\n", plural, r.id(), err)
	}
	if hash == "" {
		hash = r.Status.AppliedHash
	}
	var prev *condition
	for i := range r.Status.Conditions {
		if r.Status.Conditions[i].Type == "Ready" {
			prev = &r.Status.Conditions[i]
		}
	}
	if prev != nil && prev.Status == c.Status && prev.Message == c.Message && r.Status.ObservedGeneration == r.Metadata.Generation && r.Status.AppliedHash == hash {
		return
	}
	c.LastTransitionTime = time.Now().UTC().Format(time.RFC3339)
	if prev != nil && prev.Status == c.Status {
		c.LastTransitionTime = prev.LastTransitionTime
	}
	patch := map[string]any{"status": map[string]any{"observedGeneration": r.Metadata.Generation, "appliedHash": hash, "conditions": []condition{c}}}
	if perr := o.k.do(ctx, http.MethodPatch, o.path(plural, r.Metadata.Namespace, r.Metadata.Name)+"/status", "application/merge-patch+json", patch, nil); perr != nil {
		fmt.Fprintf(o.a.c.stdout, "%s %s: updating status: %v\n", plural, r.id(), perr)
	}
}

// ensureFinalizer adds the finalizer; it reports false when the
// resource changed and must be read again.
func (o *operator) ensureFinalizer(ctx context.Context, plural string, r *resource) bool {
	if slices.Contains(r.Metadata.Finalizers, finalizer) {
		return true
	}
	if err := o.setFinalizers(ctx, plural, r, append(slices.Clone(r.Metadata.Finalizers), finalizer)); err != nil {
		fmt.Fprintf(o.a.c.stdout, "%s %s: adding the finalizer: %v\n", plural, r.id(), err)
		return false
	}
	return true
}

// release removes the finalizer once the gateway is cleaned up.
func (o *operator) release(ctx context.Context, plural string, r *resource) {
	fins := slices.DeleteFunc(slices.Clone(r.Metadata.Finalizers), func(f string) bool { return f == finalizer })
	if err := o.setFinalizers(ctx, plural, r, fins); err != nil && !errors.Is(err, errKubeNotFound) {
		fmt.Fprintf(o.a.c.stdout, "%s %s: removing the finalizer: %v\n", plural, r.id(), err)
	}
}

// reconcile runs one pass: configuration objects first, since the
// policies of organizations name them.
func (o *operator) reconcile(ctx context.Context) error {
	objects, err := o.list(ctx, "gatewayconfigobjects")
	if err != nil {
		return fmt.Errorf("listing GatewayConfigObjects: %w", err)
	}
	orgs, err := o.list(ctx, "gatewayorganizations")
	if err != nil {
		return fmt.Errorf("listing GatewayOrganizations: %w", err)
	}
	if err := o.reconcileObjects(ctx, objects); err != nil {
		return err
	}
	return o.reconcileOrgs(ctx, orgs)
}

func (o *operator) reconcileObjects(ctx context.Context, items []*resource) error {
	const plural = "gatewayconfigobjects"
	out, err := o.a.c.do(http.MethodGet, "/admin/v1/config", nil)
	if err != nil {
		return err
	}
	stored := map[string]map[string]any{}
	entries, _ := out["data"].([]any)
	for _, e := range entries {
		m, _ := e.(map[string]any)
		kind, _ := m["kind"].(string)
		name, _ := m["name"].(string)
		stored[kind+"/"+name] = m
	}
	owners := map[string]string{}
	// Apply in kind order, so models come after the providers they name.
	slices.SortStableFunc(items, func(a, b *resource) int {
		ka, _ := a.Spec["kind"].(string)
		kb, _ := b.Spec["kind"].(string)
		return slices.Index(kindOrder, ka) - slices.Index(kindOrder, kb)
	})
	for _, r := range items {
		kind, _ := r.Spec["kind"].(string)
		name, _ := r.Spec["name"].(string)
		if name == "" {
			name = r.Metadata.Name
		}
		key := kind + "/" + name
		cur := stored[key]
		if r.Metadata.DeletionTimestamp != nil {
			if !slices.Contains(r.Metadata.Finalizers, finalizer) {
				continue
			}
			var err error
			switch src, _ := cur["source"].(string); src {
			case "store":
				_, err = o.a.do("delete", key, http.MethodDelete, "/admin/v1/config/"+kind+"/"+url.PathEscape(name), nil)
			case "changed", "deleted":
				// The config file's object applies again.
				_, err = o.a.do("reset", key, http.MethodPost, "/admin/v1/config/"+kind+"/"+url.PathEscape(name)+"/reset", nil)
			}
			if err != nil {
				o.setStatus(ctx, plural, r, "", err)
				continue
			}
			o.release(ctx, plural, r)
			continue
		}
		if !slices.Contains(kindOrder, kind) {
			o.setStatus(ctx, plural, r, "", fmt.Errorf("spec.kind %q is not one of %s", kind, strings.Join(kindOrder, ", ")))
			continue
		}
		if prev, ok := owners[key]; ok {
			o.setStatus(ctx, plural, r, "", fmt.Errorf("%s is already managed by %s", key, prev))
			continue
		}
		owners[key] = r.id()
		if !o.ensureFinalizer(ctx, plural, r) {
			continue
		}
		spec, secretPaths, err := o.objectSpec(ctx, r)
		if err != nil {
			o.setStatus(ctx, plural, r, "", err)
			continue
		}
		raw, _ := json.Marshal(spec)
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		src, _ := cur["source"].(string)
		// Secrets read "set", so drift is found in everything else, and
		// a changed secret by the hash.
		same := (src == "store" || src == "changed") && hash == r.Status.AppliedHash &&
			sameJSON(masked(kind, cur["spec"], secretPaths), masked(kind, spec, secretPaths))
		if !same {
			if _, err := o.a.do("put", key, http.MethodPut, "/admin/v1/config/"+kind+"/"+url.PathEscape(name), spec); err != nil {
				o.setStatus(ctx, plural, r, "", err)
				continue
			}
		}
		o.setStatus(ctx, plural, r, hash, nil)
	}
	return nil
}

// objectSpec is a resource's spec for the gateway, with each secretRef
// read from a Secret in the resource's namespace and set at its path.
func (o *operator) objectSpec(ctx context.Context, r *resource) (map[string]any, [][]string, error) {
	spec, _ := r.Spec["spec"].(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	spec = deepCopy(spec).(map[string]any)
	refs, _ := r.Spec["secretRefs"].([]any)
	var paths [][]string
	for i, ref := range refs {
		m, _ := ref.(map[string]any)
		p, _ := m["path"].(string)
		sel, _ := m["secretKeyRef"].(map[string]any)
		sname, _ := sel["name"].(string)
		skey, _ := sel["key"].(string)
		if p == "" || sname == "" || skey == "" {
			return nil, nil, fmt.Errorf("secretRefs[%d] needs path, secretKeyRef.name and secretKeyRef.key", i)
		}
		var sec struct {
			Data map[string]string `json:"data"`
		}
		err := o.k.do(ctx, http.MethodGet, "/api/v1/namespaces/"+url.PathEscape(r.Metadata.Namespace)+"/secrets/"+url.PathEscape(sname), "", nil, &sec)
		if err != nil {
			return nil, nil, fmt.Errorf("secretRefs[%d]: reading Secret %s: %w", i, sname, err)
		}
		enc, ok := sec.Data[skey]
		if !ok {
			return nil, nil, fmt.Errorf("secretRefs[%d]: Secret %s has no key %q", i, sname, skey)
		}
		val, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, nil, fmt.Errorf("secretRefs[%d]: %w", i, err)
		}
		path := strings.Split(p, ".")
		if err := setPath(spec, path, string(val)); err != nil {
			return nil, nil, fmt.Errorf("secretRefs[%d]: %w", i, err)
		}
		paths = append(paths, path)
	}
	return spec, paths, nil
}

func setPath(m map[string]any, path []string, v any) error {
	for i, k := range path {
		if i == len(path)-1 {
			m[k] = v
			return nil
		}
		next, ok := m[k].(map[string]any)
		if !ok {
			if m[k] != nil {
				return fmt.Errorf("%s is not an object", strings.Join(path[:i+1], "."))
			}
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	return nil
}

// masked replaces secrets with "set", as the admin API shows them.
func masked(kind string, v any, extra [][]string) any {
	v = deepCopy(jsonable(v))
	var walk func(v any, path []string) any
	walk = func(v any, path []string) any {
		secret := config.IsSecretPath(kind, path) || slices.ContainsFunc(extra, func(p []string) bool { return slices.Equal(p, path) })
		if secret && v != nil {
			return "set"
		}
		if m, ok := v.(map[string]any); ok {
			for k, x := range m {
				m[k] = walk(x, append(slices.Clone(path), k))
			}
		}
		return v
	}
	return walk(v, nil)
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = deepCopy(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopy(e)
		}
		return out
	}
	return v
}

func (o *operator) reconcileOrgs(ctx context.Context, items []*resource) error {
	const plural = "gatewayorganizations"
	have, err := o.a.c.all("/admin/v1/orgs")
	if err != nil {
		return err
	}
	byName := map[string]map[string]any{}
	for _, org := range have {
		byName[strings.ToLower(org["name"].(string))] = org
	}
	owners := map[string]string{}
	for _, r := range items {
		name, _ := r.Spec["name"].(string)
		if name == "" {
			name = r.Metadata.Name
		}
		cur := byName[strings.ToLower(name)]
		if r.Metadata.DeletionTimestamp != nil {
			if !slices.Contains(r.Metadata.Finalizers, finalizer) {
				continue
			}
			if cur != nil {
				if err := o.a.deleteOrg(cur); err != nil {
					o.setStatus(ctx, plural, r, "", err)
					continue
				}
			}
			o.release(ctx, plural, r)
			continue
		}
		if prev, ok := owners[strings.ToLower(name)]; ok {
			o.setStatus(ctx, plural, r, "", fmt.Errorf("organization %s is already managed by %s", name, prev))
			continue
		}
		owners[strings.ToLower(name)] = r.id()
		doc := maps.Clone(r.Spec)
		delete(doc, "name")
		docs, err := parseOrgs(map[string]map[string]any{name: jsonable(deepCopy(doc)).(map[string]any)})
		if err != nil {
			o.setStatus(ctx, plural, r, "", err)
			continue
		}
		if !o.ensureFinalizer(ctx, plural, r) {
			continue
		}
		// The resource owns its organization: teams and Cedar policies
		// it leaves out are removed.
		o.setStatus(ctx, plural, r, "", o.a.applyOrg(name, docs[name], cur, true))
	}
	return nil
}

// runOperator reconciles until it is stopped, or once with -once.
func (c *client) runOperator(args []string) error {
	fs := flag.NewFlagSet("operator", flag.ContinueOnError)
	namespace := fs.String("namespace", os.Getenv("GOG_WATCH_NAMESPACE"), "namespace to watch; empty watches all")
	interval := fs.Duration("interval", 15*time.Second, "time between reconcile passes")
	once := fs.Bool("once", false, "reconcile once and exit")
	kubeURL := fs.String("kube-url", "", "Kubernetes API URL without authentication, such as kubectl proxy's; default: the pod's service account")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var k *kube
	if *kubeURL != "" {
		k = &kube{base: strings.TrimRight(*kubeURL, "/"), http: &http.Client{Timeout: 30 * time.Second}}
	} else {
		var err error
		if k, err = inCluster(); err != nil {
			return err
		}
	}
	o := &operator{k: k, a: &applier{c: c}, namespace: *namespace}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		if err := o.reconcile(ctx); err != nil {
			if *once {
				return err
			}
			fmt.Fprintf(c.stdout, "reconcile: %v\n", err)
		}
		if *once {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(*interval):
		}
	}
}
