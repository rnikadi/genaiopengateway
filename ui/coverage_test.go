package ui

import (
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

// TestEverySettingHasAControl fails when a config setting is neither a
// startup setting nor named by a control in the UI (ADR 0027): every
// runtime setting is changed from the admin UI. A control names its
// setting with data-setting, or a field list names it, as a string
// literal of its path without map keys or list indexes, such as
// 'models.targets.price.per_image'.
func TestEverySettingHasAControl(t *testing.T) {
	var src strings.Builder
	err := fs.WalkDir(Files(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "vendor/") || !strings.HasSuffix(p, ".js") {
			return err
		}
		b, err := fs.ReadFile(Files(), p)
		src.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	text := src.String()
	var missing []string
	for _, p := range settingPaths(reflect.TypeOf(config.Config{}), "") {
		if startup(p) {
			continue
		}
		if !regexp.MustCompile(`['"]` + regexp.QuoteMeta(p) + `['"]`).MatchString(text) {
			missing = append(missing, p)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d settings have no control in the admin UI; add one with data-setting=\"<path>\", or list a startup setting in config.StartupPaths:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}

func startup(p string) bool {
	return slices.ContainsFunc(config.StartupPaths, func(s string) bool { return p == s || strings.HasPrefix(p, s+".") })
}

// settingPaths lists the leaf settings of a config type by yaml path.
func settingPaths(t reflect.Type, path string) []string {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || t == reflect.TypeOf(config.Duration(0)) {
		return []string{path}
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("yaml"), ",")
		if tag[0] == "-" || !f.IsExported() {
			continue
		}
		if slices.Contains(tag[1:], "inline") {
			out = append(out, settingPaths(f.Type, path)...)
			continue
		}
		p := tag[0]
		if path != "" {
			p = path + "." + p
		}
		out = append(out, settingPaths(f.Type, p)...)
	}
	return out
}
