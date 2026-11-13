package secretbox

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func key(t *testing.T) []byte {
	k := make([]byte, KeySize)
	_, _ = rand.Read(k)
	return k
}

func TestSealOpen(t *testing.T) {
	b, err := New(key(t))
	if err != nil {
		t.Fatal(err)
	}
	sealed := b.Seal([]byte("hello"), []byte("row-1"))
	if strings.Contains(sealed, "hello") || !b.Current(sealed) {
		t.Fatalf("sealed = %q", sealed)
	}
	if got, err := b.Open(sealed, []byte("row-1")); err != nil || string(got) != "hello" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if b.Seal([]byte("hello"), []byte("row-1")) == sealed {
		t.Fatal("nonces repeat")
	}
	// Bound to its row.
	if _, err := b.Open(sealed, []byte("row-2")); !errors.Is(err, ErrOpen) {
		t.Fatalf("open with other data: %v", err)
	}
	// Tampering is detected.
	tampered := sealed[:len(sealed)-2] + "AA"
	if _, err := b.Open(tampered, []byte("row-1")); !errors.Is(err, ErrOpen) {
		t.Fatalf("open tampered: %v", err)
	}
	for _, bad := range []string{"", "nokey", "deadbeef.!!!", "deadbeef.AAAA"} {
		if _, err := b.Open(bad, nil); !errors.Is(err, ErrOpen) {
			t.Fatalf("open %q: %v", bad, err)
		}
	}
}

func TestRotation(t *testing.T) {
	oldKey, newKey := key(t), key(t)
	old, _ := New(oldKey)
	sealed := old.Seal([]byte("secret"), nil)
	rotated, _ := New(newKey, oldKey)
	if got, err := rotated.Open(sealed, nil); err != nil || !bytes.Equal(got, []byte("secret")) {
		t.Fatalf("open with a previous key = %q, %v", got, err)
	}
	if rotated.Current(sealed) || !rotated.Current(rotated.Seal(nil, nil)) {
		t.Fatal("Current does not tell keys apart")
	}
	other, _ := New(key(t))
	if _, err := other.Open(sealed, nil); !errors.Is(err, ErrOpen) {
		t.Fatal("a different key opened the value")
	}
}

func TestParseKey(t *testing.T) {
	k := key(t)
	for _, enc := range []string{base64.StdEncoding.EncodeToString(k), base64.RawURLEncoding.EncodeToString(k) + "\n"} {
		if got, err := ParseKey(enc); err != nil || !bytes.Equal(got, k) {
			t.Fatalf("ParseKey(%q) = %v", enc, err)
		}
	}
	for _, bad := range []string{"", "short", base64.StdEncoding.EncodeToString(k[:16])} {
		if _, err := ParseKey(bad); err == nil {
			t.Fatalf("ParseKey(%q) accepted a bad key", bad)
		}
	}
}
