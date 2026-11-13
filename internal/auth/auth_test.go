package auth

import (
	"strings"
	"testing"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/policy"
)

func TestNewKey(t *testing.T) {
	secret, prefix, hash, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, KeyPrefix) || len(secret) != len(KeyPrefix)+keyRandomLen {
		t.Fatalf("bad secret %q", secret)
	}
	if !strings.HasPrefix(secret, prefix) || len(prefix) != DisplayPrefixLen {
		t.Fatalf("bad prefix %q", prefix)
	}
	if hash != Hash(secret) || len(hash) != 64 {
		t.Fatalf("bad hash %q", hash)
	}
	other, _, _, _ := NewKey()
	if other == secret {
		t.Fatal("keys repeat")
	}
}

func TestDirectoryLookupAndPolicies(t *testing.T) {
	d := NewDirectory(StaticPrincipals([]config.StaticKey{
		{Name: "a", Key: "static-key-aaaaaaaa", Policy: policy.Policy{AllowedModels: []string{"chat"}}},
	}))
	p, ok := d.Lookup("static-key-aaaaaaaa")
	if !ok || p.KeyName != "a" || p.KeyPrefix != "static-key-" {
		t.Fatalf("lookup = %+v, %v", p, ok)
	}
	if !p.AllowsModel("chat") || p.AllowsModel("embed") {
		t.Fatal("allow list not applied")
	}
	if _, ok := d.Lookup("static-key-bbbbbbbb"); ok {
		t.Fatal("unknown key resolved")
	}
	if _, ok := d.Lookup(""); ok {
		t.Fatal("empty key resolved")
	}
}

func TestAllowsModelChecksEveryLevel(t *testing.T) {
	p := &Principal{Policies: [3]policy.Policy{
		{AllowedModels: []string{"a", "b"}},
		{},
		{AllowedModels: []string{"b", "c"}},
	}}
	for model, want := range map[string]bool{"a": false, "b": true, "c": false} {
		if got := p.AllowsModel(model); got != want {
			t.Errorf("AllowsModel(%q) = %v, want %v", model, got, want)
		}
	}
}
