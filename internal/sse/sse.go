// Package sse observes server-sent event streams as they pass through
// the gateway, without buffering or changing them.
package sse

import (
	"bytes"
	"io"
)

// maxLine bounds memory for a single event line. Longer lines are passed
// through to the client but not inspected.
const maxLine = 1 << 20

// Tap returns a reader that yields exactly the bytes of r and calls
// onData with the payload of every complete "data:" line it sees.
func Tap(r io.ReadCloser, onData func([]byte)) io.ReadCloser {
	return &tap{r: r, onData: onData}
}

type tap struct {
	r        io.ReadCloser
	onData   func([]byte)
	line     []byte
	overflow bool
}

func (t *tap) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.scan(p[:n])
	if err == io.EOF {
		t.flush()
	}
	return n, err
}

func (t *tap) Close() error { return t.r.Close() }

func (t *tap) scan(b []byte) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			t.append(b)
			return
		}
		t.append(b[:i])
		t.flush()
		b = b[i+1:]
	}
}

func (t *tap) append(b []byte) {
	if t.overflow {
		return
	}
	if len(t.line)+len(b) > maxLine {
		t.overflow = true
		t.line = t.line[:0]
		return
	}
	t.line = append(t.line, b...)
}

func (t *tap) flush() {
	line := bytes.TrimSuffix(t.line, []byte("\r"))
	if !t.overflow {
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			t.onData(bytes.TrimPrefix(data, []byte(" ")))
		}
	}
	t.line = t.line[:0]
	t.overflow = false
}
