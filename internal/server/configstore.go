package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// Configuration objects stored through the admin API and merged into the
// config file (ADR 0023, docs/admin-ui-config-editing.md).

// secretSet stands for a stored secret in responses, and keeps it in
// updates.
const secretSet = "set"

// secretField is one secret value of a stored object's spec.
type secretField struct {
	Path  []string `json:"path"`
	Value string   `json:"value"`
}

func configAD(kind, name string) []byte { return []byte("config/" + kind + "/" + name) }

// merge adds stored objects to the config file. Without the file's bytes
// (a config built in code) there is nothing to merge into.
func (s *Server) merge(file *config.Config, objects []config.Object) (*config.Config, error) {
	if len(objects) == 0 || file.Raw == nil {
		return file, nil
	}
	cfg, _, err := config.ParseWith(file.Raw, objects)
	return cfg, err
}

// refreshConfig applies stored objects when their revision has moved. A
// set that does not apply is reported and the running config is kept.
func (s *Server) refreshConfig(ctx context.Context) error {
	rev, err := s.store.ConfigRevision(ctx)
	if err != nil {
		return err
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if rev == s.cfgRev {
		return nil
	}
	rows, rev, err := s.store.ListConfigObjects(ctx)
	if err != nil {
		return err
	}
	s.cfgRev = rev
	objects, err := s.openObjects(rows)
	if err == nil {
		err = s.applyObjects(objects)
	}
	if err != nil {
		s.cfgErr = err.Error()
		s.log.Error("stored configuration objects do not apply; keeping the running config", "revision", rev, "err", err)
		return nil
	}
	s.cfgErr = ""
	return nil
}

// applyObjects merges objects into the file and installs the result.
// The caller holds cfgMu.
func (s *Server) applyObjects(objects []config.Object) error {
	cfg, err := s.merge(s.file, objects)
	if err != nil {
		return err
	}
	if err := s.install(cfg); err != nil {
		return err
	}
	s.applied = objects
	return nil
}

// openObjects decrypts stored objects into complete specs.
func (s *Server) openObjects(rows []*store.ConfigObject) ([]config.Object, error) {
	out := make([]config.Object, 0, len(rows))
	for _, row := range rows {
		spec, err := s.fullSpec(row)
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(spec)
		out = append(out, config.Object{Kind: row.Kind, Name: row.Name, Spec: raw})
	}
	return out, nil
}

// fullSpec returns a stored object's spec with its secrets put back.
func (s *Server) fullSpec(row *store.ConfigObject) (map[string]any, error) {
	var spec map[string]any
	if err := json.Unmarshal([]byte(row.Spec), &spec); err != nil {
		return nil, fmt.Errorf("%s %q: %w", row.Kind, row.Name, err)
	}
	secrets, err := s.openSecrets(row)
	if err != nil {
		return nil, err
	}
	for _, f := range secrets {
		setPath(spec, f.Path, f.Value)
	}
	return spec, nil
}

func (s *Server) openSecrets(row *store.ConfigObject) ([]secretField, error) {
	if row.Secrets == "" {
		return nil, nil
	}
	box, err := secretBox(s.file.Secrets)
	if err != nil {
		return nil, fmt.Errorf("%s %q has secrets: %w", row.Kind, row.Name, err)
	}
	plain, err := box.Open(row.Secrets, configAD(row.Kind, row.Name))
	if err != nil {
		return nil, fmt.Errorf("%s %q: decrypting secrets: %w", row.Kind, row.Name, err)
	}
	var out []secretField
	return out, json.Unmarshal(plain, &out)
}

// --- admin API ---

type configEntry struct {
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Source    string         `json:"source"`
	Shadowed  bool           `json:"shadowed,omitempty"`
	Spec      map[string]any `json:"spec,omitempty"`
	UpdatedAt *time.Time     `json:"updated_at,omitempty"`
	UpdatedBy string         `json:"updated_by,omitempty"`
}

// fileNames lists the names each kind has in the config file.
func fileNames(file *config.Config) map[string][]string {
	var hooks []string
	for _, w := range file.Alerts.Webhooks {
		hooks = append(hooks, w.Name)
	}
	return map[string][]string{
		config.KindProviders:         slices.Sorted(maps.Keys(file.Providers)),
		config.KindModels:            slices.Sorted(maps.Keys(file.Models)),
		config.KindGuardrails:        slices.Sorted(maps.Keys(file.Guardrails)),
		config.KindMCPServers:        slices.Sorted(maps.Keys(file.MCP.Servers)),
		config.KindMCPVirtualServers: slices.Sorted(maps.Keys(file.MCP.VirtualServers)),
		config.KindAlertWebhooks:     hooks,
	}
}

func (s *Server) listConfig(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	rows, rev, err := s.store.ListConfigObjects(r.Context())
	if err != nil {
		return err
	}
	s.cfgMu.Lock()
	file, cfgErr := s.file, s.cfgErr
	s.cfgMu.Unlock()
	names := fileNames(file)
	data := []configEntry{}
	for _, kind := range config.Kinds {
		for _, name := range names[kind] {
			data = append(data, configEntry{Kind: kind, Name: name, Source: "file"})
		}
		for _, row := range rows {
			if row.Kind != kind {
				continue
			}
			spec, err := maskedSpec(row)
			if err != nil {
				return err
			}
			at := row.UpdatedAt
			data = append(data, configEntry{
				Kind: kind, Name: row.Name, Source: "store", Shadowed: slices.Contains(names[kind], row.Name),
				Spec: spec, UpdatedAt: &at, UpdatedBy: row.UpdatedBy,
			})
		}
	}
	out := map[string]any{"data": data, "revision": rev, "kinds": config.Kinds}
	if cfgErr != "" {
		out["error"] = cfgErr
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// maskedSpec is a stored spec as the API shows it: secrets are stored as
// "set" in the spec and their values beside it, encrypted.
func maskedSpec(row *store.ConfigObject) (map[string]any, error) {
	var spec map[string]any
	return spec, json.Unmarshal([]byte(row.Spec), &spec)
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	if err := checkObjectRef(kind, name); err != nil {
		return err
	}
	var spec map[string]any
	if err := decode(r, &spec); err != nil {
		return err
	}
	if spec == nil {
		return badRequest("the body must be a JSON object: the %s spec", kind)
	}
	ctx := r.Context()
	s.cfgMu.Lock()
	file := s.file
	s.cfgMu.Unlock()
	if slices.Contains(fileNames(file)[kind], name) {
		return &apiError{http.StatusConflict, "conflict", fmt.Sprintf("%s %q is defined in the config file; change it there", kind, name)}
	}
	var old []secretField
	prev, err := s.store.GetConfigObject(ctx, kind, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	default:
		if old, err = s.openSecrets(prev); err != nil {
			return err
		}
	}
	secrets, err := takeSecrets(kind, spec, old)
	if err != nil {
		return err
	}
	for _, f := range secrets {
		setPath(spec, f.Path, f.Value)
	}
	rawFull, _ := json.Marshal(spec)
	for _, f := range secrets {
		setPath(spec, f.Path, secretSet)
	}
	if err := config.CheckSpec(kind, rawFull); err != nil {
		return badRequest("%s", err)
	}
	if err := s.validateObjects(ctx, file, config.Object{Kind: kind, Name: name, Spec: rawFull}, false); err != nil {
		return err
	}

	row := &store.ConfigObject{Kind: kind, Name: name, UpdatedBy: operatorFrom(ctx).actor()}
	if len(secrets) > 0 {
		box, err := secretBox(file.Secrets)
		if err != nil {
			return badRequest("storing secrets needs secrets.encryption_key in the config file: %s", err)
		}
		plain, _ := json.Marshal(secrets)
		row.Secrets = box.Seal(plain, configAD(kind, name))
	}
	rawSpec, _ := json.Marshal(spec)
	row.Spec = string(rawSpec)
	created, err := s.store.PutConfigObject(ctx, row)
	if err != nil {
		return err
	}
	action, status := "config.update", http.StatusOK
	if created {
		action, status = "config.create", http.StatusCreated
	}
	var before map[string]any
	if prev != nil {
		before, _ = maskedSpec(prev)
	}
	if err := s.audit(ctx, action, kind, name, "", map[string]any{"changed": changedPaths(before, spec)}); err != nil {
		return err
	}
	masked, err := maskedSpec(row)
	if err != nil {
		return err
	}
	at := row.UpdatedAt
	writeJSON(w, status, configEntry{Kind: kind, Name: name, Source: "store", Spec: masked, UpdatedAt: &at, UpdatedBy: row.UpdatedBy})
	return nil
}

func (s *Server) deleteConfig(w http.ResponseWriter, r *http.Request) error {
	if err := requireOwner(r); err != nil {
		return err
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	if err := checkObjectRef(kind, name); err != nil {
		return err
	}
	ctx := r.Context()
	if _, err := s.store.GetConfigObject(ctx, kind, name); err != nil {
		return err
	}
	s.cfgMu.Lock()
	file := s.file
	s.cfgMu.Unlock()
	if err := s.validateObjects(ctx, file, config.Object{Kind: kind, Name: name}, true); err != nil {
		return err
	}
	if err := s.store.DeleteConfigObject(ctx, kind, name); err != nil {
		return err
	}
	if err := s.audit(ctx, "config.delete", kind, name, "", nil); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func checkObjectRef(kind, name string) error {
	if kind == config.KindSettings {
		if name != config.SettingsName {
			return &apiError{http.StatusNotFound, "not_found", fmt.Sprintf("settings are named %q", config.SettingsName)}
		}
		return nil
	}
	if !slices.Contains(config.Kinds, kind) {
		return &apiError{http.StatusNotFound, "not_found", fmt.Sprintf("unknown kind %q", kind)}
	}
	if !config.ValidObjectName(name) {
		return badRequest("a name is 1 to 63 lowercase letters, digits, '.', '_' or '-', starting with a letter or digit")
	}
	return nil
}

// validateObjects checks that the stored objects, with o put in or taken
// out, still make a config the gateway can run.
func (s *Server) validateObjects(ctx context.Context, file *config.Config, o config.Object, remove bool) error {
	rows, _, err := s.store.ListConfigObjects(ctx)
	if err != nil {
		return err
	}
	rows = slices.DeleteFunc(rows, func(row *store.ConfigObject) bool { return row.Kind == o.Kind && row.Name == o.Name })
	objects, err := s.openObjects(rows)
	if err != nil {
		return err
	}
	if !remove {
		objects = append(objects, o)
	}
	cfg, err := s.merge(file, objects)
	if err == nil {
		_, err = s.build(cfg)
	}
	if err != nil {
		if remove {
			return badRequest("deleting %s %q: %s", o.Kind, o.Name, err)
		}
		return badRequest("%s", err)
	}
	return nil
}

// takeSecrets removes the secret values from spec and returns them. A
// secret sent as "set" keeps the stored value, and so does one left out
// unless dropped says otherwise; "" removes it.
func takeSecrets(kind string, spec map[string]any, old []secretField) ([]secretField, error) {
	var out []secretField
	oldValue := func(path []string) (string, bool) {
		for _, f := range old {
			if slices.Equal(f.Path, path) {
				return f.Value, true
			}
		}
		return "", false
	}
	for _, pattern := range config.SecretFields[kind] {
		for _, path := range matchPaths(spec, pattern) {
			v, ok := getPath(spec, path).(string)
			if !ok {
				return nil, badRequest("%s must be a string", joinPath(path))
			}
			deletePath(spec, path)
			switch v {
			case "":
			case secretSet:
				prev, ok := oldValue(path)
				if !ok {
					return nil, badRequest("%s is %q but no value is stored", joinPath(path), secretSet)
				}
				out = append(out, secretField{path, prev})
			default:
				out = append(out, secretField{path, v})
			}
		}
	}
	// Secrets left out are kept.
	for _, f := range old {
		if slices.ContainsFunc(out, func(n secretField) bool { return slices.Equal(n.Path, f.Path) }) || dropped(spec, kind, f.Path) {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// dropped reports whether an update left out a stored secret on purpose:
// it sent the headers map without that header, or left out the field
// holding the secret.
func dropped(spec map[string]any, kind string, path []string) bool {
	parent := len(path) == 1 || getPath(spec, path[:len(path)-1]) != nil
	for _, p := range config.SecretFields[kind] {
		if p[len(p)-1] == "*" && len(p) == len(path) && config.IsSecretPath(kind, path) {
			return parent
		}
	}
	return !parent
}

// matchPaths returns the concrete paths in spec that match a pattern.
func matchPaths(spec map[string]any, pattern []string) [][]string {
	var out [][]string
	var walk func(v any, prefix []string, rest []string)
	walk = func(v any, prefix []string, rest []string) {
		if len(rest) == 0 {
			out = append(out, slices.Clone(prefix))
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			return
		}
		if rest[0] == "*" {
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, k := range keys {
				walk(m[k], append(prefix, k), rest[1:])
			}
			return
		}
		if c, ok := m[rest[0]]; ok {
			walk(c, append(prefix, rest[0]), rest[1:])
		}
	}
	walk(spec, nil, pattern)
	return out
}

func getPath(spec map[string]any, path []string) any {
	var v any = spec
	for _, k := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		if v, ok = m[k]; !ok {
			return nil
		}
	}
	return v
}

func setPath(spec map[string]any, path []string, value any) {
	m := spec
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = value
}

func deletePath(spec map[string]any, path []string) {
	if m, ok := getPath(spec, path[:len(path)-1]).(map[string]any); ok {
		delete(m, path[len(path)-1])
	}
}

func joinPath(path []string) string {
	out := ""
	for i, p := range path {
		if i > 0 {
			out += "."
		}
		out += p
	}
	return out
}

// changedPaths lists the dotted paths whose values differ between two
// specs, sorted. Secrets compare as "set", so a new secret value shows
// only when one is added or removed.
func changedPaths(before, after map[string]any) []string {
	flat := func(spec map[string]any) map[string]string {
		out := map[string]string{}
		var walk func(prefix string, v any)
		walk = func(prefix string, v any) {
			if m, ok := v.(map[string]any); ok && len(m) > 0 {
				for k, sub := range m {
					p := k
					if prefix != "" {
						p = prefix + "." + k
					}
					walk(p, sub)
				}
				return
			}
			if prefix == "" {
				return
			}
			b, _ := json.Marshal(v)
			out[prefix] = string(b)
		}
		walk("", spec)
		return out
	}
	a, b := flat(before), flat(after)
	changed := []string{}
	for k, v := range b {
		if a[k] != v {
			changed = append(changed, k)
		}
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			changed = append(changed, k)
		}
	}
	slices.Sort(changed)
	return changed
}
