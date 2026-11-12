package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, dir, key string) string {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	yml := fmt.Sprintf("admin: {token: %q}\nstore: {driver: sqlite, dsn: %q}\nsecrets: {encryption_key: %q}\n",
		strings.Repeat("a", 40), "file:"+filepath.Join(dir, "gw.db"), key)
	if err := os.WriteFile(p, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBackupAndRestoreCommands(t *testing.T) {
	key := "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="
	src := writeConfig(t, t.TempDir(), key)
	archive := filepath.Join(t.TempDir(), "b.gogb")
	var out, errb bytes.Buffer
	if ok, code := subcommand([]string{"backup", "-config", src, "-out", archive}, &out, &errb); !ok || code != 0 {
		t.Fatalf("backup = %v %d: %s", ok, code, errb.String())
	}
	if fi, err := os.Stat(archive); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("archive = %v, %v", fi, err)
	}

	// The wrong key is refused before anything is written.
	other := writeConfig(t, t.TempDir(), "ZmVkY2JhOTg3NjU0MzIxMGZlZGNiYTk4NzY1NDMyMTA=")
	errb.Reset()
	if _, code := subcommand([]string{"restore", "-config", other, archive}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "encryption_key") {
		t.Fatalf("restore with another key = %d: %s", code, errb.String())
	}

	dst := writeConfig(t, t.TempDir(), key)
	out.Reset()
	if _, code := subcommand([]string{"restore", "-config", dst, archive}, &out, &errb); code != 0 || !strings.Contains(out.String(), "Restored") {
		t.Fatalf("restore = %d: %s %s", code, out.String(), errb.String())
	}
	out.Reset()
	if _, code := subcommand([]string{"restore", "-check", archive}, &out, &errb); code != 0 || !strings.Contains(out.String(), "schema") {
		t.Fatalf("check = %d: %s", code, out.String())
	}
	if ok, _ := subcommand([]string{"serve"}, &out, &errb); ok {
		t.Fatal("an unknown word ran as a command")
	}
}
