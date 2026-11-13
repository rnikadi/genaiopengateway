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

func TestDropRemovesMatchingEventsOnly(t *testing.T) {
	in := "data: {\"a\":1}\n\nevent: x\r\ndata: drop-me\r\n\r\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"
	r := Drop(io.NopCloser(iotest.OneByteReader(strings.NewReader(in))), func(d []byte) bool { return string(d) == "drop-me" })
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	want := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestDropPassesOversizedAndUnterminatedEvents(t *testing.T) {
	big := "data: " + strings.Repeat("x", 200<<10) + "\n\n"
	in := big + "data: drop-me\n\ndata: tail-without-blank-line"
	got, _ := io.ReadAll(Drop(io.NopCloser(strings.NewReader(in)), func(d []byte) bool { return string(d) == "drop-me" }))
	if string(got) != big+"data: tail-without-blank-line" {
		t.Fatalf("got %d bytes, want the big event and the tail", len(got))
	}
}
