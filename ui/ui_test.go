package ui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

// TestReferencesResolve checks that every file the UI loads is embedded,
// and that pages keep to the content security policy.
func TestReferencesResolve(t *testing.T) {
	files := Files()
	refs := regexp.MustCompile(`(?:from\s+|import\s*\(\s*|(?:src|href)=)['"](\.{0,2}/?[\w./-]+\.(?:js|css|svg))['"]`)
	err := fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, "vendor/") {
			return err
		}
		b, err := fs.ReadFile(files, p)
		if err != nil {
			return err
		}
		src := string(b)
		for _, m := range refs.FindAllStringSubmatch(src, -1) {
			target := path.Join(path.Dir(p), m[1])
			if _, err := fs.Stat(files, target); err != nil {
				t.Errorf("%s references %s, which is not embedded", p, m[1])
			}
		}
		if strings.HasSuffix(p, ".html") && regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]|<style|\sstyle=|\son\w+=`).MatchString(src) {
			t.Errorf("%s has inline script or style, which the content security policy blocks", p)
		}
		if strings.Contains(src, "dangerouslySetInnerHTML") || strings.Contains(src, "innerHTML") {
			t.Errorf("%s sets HTML directly; render API data as text", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Stat(files, "index.html"); err != nil {
		t.Fatal("index.html is missing")
	}
}
