package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
)

func TestSAMLKeygen(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "sp.crt"), filepath.Join(dir, "sp.key")
	var out, errb bytes.Buffer
	if ok, code := subcommand([]string{"saml-keygen", "-cert", cert, "-key", key}, &out, &errb); !ok || code != 0 {
		t.Fatalf("saml-keygen = %v %d: %s", ok, code, errb.String())
	}
	if st, err := os.Stat(key); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", st, err)
	}
	c, _ := os.ReadFile(cert)
	k, _ := os.ReadFile(key)
	m := &config.SAML{SPCertificate: string(c), SPPrivateKey: config.Secret(k)}
	if pair, err := m.SPKeyPair(); err != nil || pair == nil {
		t.Fatalf("the generated pair does not load: %v", err)
	}
	errb.Reset()
	if _, code := subcommand([]string{"saml-keygen", "-cert", cert, "-key", key}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "exists") {
		t.Fatalf("overwrote a key in use: %d %s", code, errb.String())
	}
}
