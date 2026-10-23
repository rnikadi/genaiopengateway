package redact

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func all(t *testing.T) *Redactor {
	t.Helper()
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuiltins(t *testing.T) {
	r := all(t)
	for _, tc := range []struct{ in, want string }{
		// Redacted.
		{"mail jane.doe+x@example.co.uk now", "mail [REDACTED:email] now"},
		{"card 4111 1111 1111 1111 exp", "card [REDACTED:credit_card] exp"},
		{"card 4111-1111-1111-1111", "card [REDACTED:credit_card]"},
		{"card 4111111111111111.", "card [REDACTED:credit_card]."},
		{"cards 4111 1111 1111 1111 12", "cards [REDACTED:credit_card] 12"},
		{"iban DE89 3704 0044 0532 0130 00 ok", "iban [REDACTED:iban] ok"},
		{"iban GB82WEST12345698765432", "iban [REDACTED:iban]"},
		{"ssn 123-45-6789", "ssn [REDACTED:us_ssn]"},
		{"call +44 20 7946 0958 today", "call [REDACTED:phone] today"},
		{"call (415) 555-0132", "call [REDACTED:phone]"},
		{"call 415-555-0132", "call [REDACTED:phone]"},
		{"key sk-proj-abcdefghijklmnopqrstuvwxyz012345", "key [REDACTED:secret]"},
		{"key sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "key [REDACTED:secret]"},
		{"aws AKIAIOSFODNN7EXAMPLE", "aws [REDACTED:secret]"},
		{"gh ghp_abcdefghijklmnopqrstuvwxyz0123456789", "gh [REDACTED:secret]"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U", "jwt [REDACTED:secret]"},
		{"-----BEGIN RSA PRIVATE KEY-----\nMIIabc\n-----END RSA PRIVATE KEY----- done", "[REDACTED:secret] done"},
		// Left alone.
		{"card 4111 1111 1111 1112", "card 4111 1111 1111 1112"},
		{"iban GB82WEST12345698765433", "iban GB82WEST12345698765433"},
		{"ssn 000-12-3456 and 666-12-3456 and 900-12-3456", "ssn 000-12-3456 and 666-12-3456 and 900-12-3456"},
		{"at 2026-10-23 09:32:14 build 1.24.7", "at 2026-10-23 09:32:14 build 1.24.7"},
		{"order 1234567890 shipped", "order 1234567890 shipped"},
		{"max_tokens 4096, user@host", "max_tokens 4096, user@host"},
	} {
		counts := Counts{}
		if got := r.String(tc.in, counts); got != tc.want {
			t.Errorf("String(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBodyJSON(t *testing.T) {
	r := all(t)
	in := `{"model":"chat","max_tokens":100,"api_key":"plain-value","metadata":{"user_token":"abc"},
		"messages":[{"role":"user","content":"I am jane@example.com <b>"}],
		"image":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="}`
	out, counts, truncated := r.Body([]byte(in), 1<<16)
	if truncated {
		t.Fatal("truncated")
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatalf("not JSON: %s", out)
	}
	if v["api_key"] != "[REDACTED:secret]" || v["metadata"].(map[string]any)["user_token"] != "[REDACTED:secret]" || v["max_tokens"] != 100.0 || v["model"] != "chat" {
		t.Fatalf("fields: %s", out)
	}
	msg := v["messages"].([]any)[0].(map[string]any)["content"]
	if msg != "I am [REDACTED:email] <b>" {
		t.Fatalf("content = %q", msg)
	}
	if img := v["image"].(string); !strings.HasPrefix(img, "[omitted: image/png, ") {
		t.Fatalf("image = %q", img)
	}
	if counts[Email] != 1 || counts[Secret] != 2 {
		t.Fatalf("counts = %v", counts)
	}
	if !strings.Contains(string(out), "<b>") {
		t.Fatal("HTML is escaped, which makes stored bodies hard to read")
	}
}

func TestBodyTruncatesAndBounds(t *testing.T) {
	r := all(t)
	long := `{"text":"` + strings.Repeat("é", 1000) + `"}`
	out, _, truncated := r.Body([]byte(long), 101)
	if !truncated || len(out) > 101 || !strings.HasPrefix(string(out), `{"text":"éé`) {
		t.Fatalf("out = %d bytes, truncated %v", len(out), truncated)
	}
	if !utf8.Valid(out) {
		t.Fatal("cut inside a character")
	}
	// Not JSON: redacted as text.
	out, counts, _ := r.Body([]byte("from bob@example.com"), 100)
	if string(out) != "from [REDACTED:email]" || counts[Email] != 1 {
		t.Fatalf("text body = %q", out)
	}
	// Huge input is cut before parsing, then redacted as text.
	huge := strings.Repeat("a@b.co ", 1000)
	out, _, truncated = r.Body([]byte(huge), 50)
	if !truncated || len(out) > 50 {
		t.Fatalf("huge: %d bytes, truncated %v", len(out), truncated)
	}
}

func TestConfig(t *testing.T) {
	r, err := New(Config{Builtin: []string{Email}, Patterns: []string{`EMP-\d{6}`}})
	if err != nil {
		t.Fatal(err)
	}
	counts := Counts{}
	got := r.String("EMP-123456 a@b.io 4111 1111 1111 1111", counts)
	if got != "[REDACTED:custom] [REDACTED:email] 4111 1111 1111 1111" || counts[Custom] != 1 {
		t.Fatalf("got %q", got)
	}
	if _, err := New(Config{Builtin: []string{"names"}}); err == nil {
		t.Fatal("unknown builtin accepted")
	}
	if _, err := New(Config{Patterns: []string{"("}}); err == nil {
		t.Fatal("bad pattern accepted")
	}
	none, _ := New(Config{Builtin: []string{}})
	if got := none.String("a@b.io", Counts{}); got != "a@b.io" {
		t.Fatal("an empty builtin list still redacted")
	}
}

func FuzzBodyStaysJSON(f *testing.F) {
	f.Add(`{"a":"jane@example.com","b":[1,"4111 1111 1111 1111"],"token":"x"}`)
	f.Add(`["+44 20 7946 0958", {"x": null}]`)
	r, _ := New(Config{})
	f.Fuzz(func(t *testing.T, in string) {
		if !json.Valid([]byte(in)) {
			return
		}
		out, _, truncated := r.Body([]byte(in), 1<<20)
		if !truncated && !json.Valid(out) {
			t.Fatalf("Body(%q) = %q is not JSON", in, out)
		}
	})
}
