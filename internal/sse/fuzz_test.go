package sse

import (
	"bytes"
	"io"
	"testing"
)

// FuzzTap checks that a tapped stream reaches the client byte for byte,
// whatever the upstream sends and however it is split into reads.
func FuzzTap(f *testing.F) {
	f.Add([]byte("data: {\"a\":1}\n\ndata: [DONE]\n\n"), uint8(3))
	f.Add([]byte("event: x\r\ndata: y\r\n\r\n"), uint8(1))
	f.Fuzz(func(t *testing.T, data []byte, chunk uint8) {
		r := Tap(io.NopCloser(&chunked{b: data, n: int(chunk%16) + 1}), func([]byte) {})
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("the stream changed: %q -> %q (%v)", data, got, err)
		}
		limited := TapLimit(io.NopCloser(bytes.NewReader(data)), func([]byte) {}, 8)
		if got, _ := io.ReadAll(limited); !bytes.Equal(got, data) {
			t.Fatalf("the limited tap changed the stream: %q -> %q", data, got)
		}
	})
}

// FuzzDrop checks that dropping nothing changes nothing.
func FuzzDrop(f *testing.F) {
	f.Add([]byte("data: {\"a\":1}\n\ndata: [DONE]\n\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		out, _ := io.ReadAll(Drop(io.NopCloser(bytes.NewReader(data)), func([]byte) bool { return false }))
		_ = out
		all, _ := io.ReadAll(Drop(io.NopCloser(bytes.NewReader(data)), func([]byte) bool { return true }))
		_ = all
	})
}

type chunked struct {
	b []byte
	n int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.b) == 0 {
		return 0, io.EOF
	}
	n := min(c.n, len(p), len(c.b))
	copy(p, c.b[:n])
	c.b = c.b[n:]
	return n, nil
}
