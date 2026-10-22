package sse

import (
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestTapPassesBytesAndSeesEveryDataLine(t *testing.T) {
	in := "event: x\r\ndata: {\"a\":1}\r\n\r\ndata:{\"b\":2}\n\n: comment\ndata: [DONE]"
	var seen []string
	// OneByteReader splits lines across reads.
	r := Tap(io.NopCloser(iotest.OneByteReader(strings.NewReader(in))), func(b []byte) {
		seen = append(seen, string(b))
	})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != in {
		t.Fatalf("bytes changed: %q", out)
	}
	want := []string{`{"a":1}`, `{"b":2}`, `[DONE]`}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("seen %q, want %q", seen, want)
	}
}

func TestTapSkipsOversizedLines(t *testing.T) {
	in := "data: " + strings.Repeat("x", maxLine+10) + "\ndata: ok\n"
	var seen []string
	out, _ := io.ReadAll(Tap(io.NopCloser(strings.NewReader(in)), func(b []byte) { seen = append(seen, string(b)) }))
	if len(out) != len(in) {
		t.Fatal("bytes lost")
	}
	if len(seen) != 1 || seen[0] != "ok" {
		t.Fatalf("seen %d lines, want only the short one", len(seen))
	}
}
