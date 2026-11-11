package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestZeroConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	c, b, err := ZeroConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Created || len(b.AdminToken.Value()) != 64 {
		t.Fatalf("bootstrap = %+v", b)
	}
	if !c.AdminEnabled() || !c.UIEnabled() || c.Store.Driver != "sqlite" || c.Limiter.Driver != "memory" || c.Secrets.EncryptionKey == "" {
		t.Fatalf("config = %+v", c)
	}
	if c.Store.DSN.Value() != "file:"+filepath.ToSlash(filepath.Join(dir, "gateway.db")) {
		t.Fatalf("dsn = %q", c.Store.DSN.Value())
	}
	fi, err := os.Stat(b.File)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("bootstrap file mode = %v, %v", fi.Mode(), err)
	}

	// A second start reuses the secrets.
	c2, b2, err := ZeroConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b2.Created || b2.AdminToken != b.AdminToken || c2.Secrets.EncryptionKey != c.Secrets.EncryptionKey {
		t.Fatal("second start generated new secrets")
	}

	// A damaged file is an error, not silently replaced.
	if err := os.WriteFile(b.File, []byte(`{"admin_token":"short"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ZeroConfig(dir); err == nil {
		t.Fatal("damaged bootstrap file accepted")
	}
}
