// Package sse observes server-sent event streams as they pass through
// the gateway, and can drop events the gateway added for itself.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// maxLine bounds memory for a single event line. Longer lines are passed
// through to the client but not inspected.
const maxLine = 1 << 20

// Tap returns a reader that yields exactly the bytes of r and calls
// onData with the payload of every complete "data:" line it sees.
func Tap(r io.ReadCloser, onData func([]byte)) io.ReadCloser {
	return TapLimit(r, onData, maxLine)
}

// TapLimit is Tap with a different bound on the lines it inspects, for
// streams whose events carry images.
func TapLimit(r io.ReadCloser, onData func([]byte), limit int) io.ReadCloser {
	return &tap{r: r, onData: onData, limit: limit}
}

type tap struct {
	r        io.ReadCloser
	onData   func([]byte)
	limit    int
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
	if len(t.line)+len(b) > t.limit {
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

// Drop returns a reader that yields the events of r except those whose
// data payload matches drop. Events are passed on whole, as soon as
// their closing blank line arrives. Lines longer than the read buffer
// pass through unexamined.
func Drop(r io.ReadCloser, drop func(data []byte) bool) io.ReadCloser {
	return &dropper{br: bufio.NewReaderSize(r, 64<<10), c: r, drop: drop}
}

type dropper struct {
	br    *bufio.Reader
	c     io.Closer
	drop  func([]byte) bool
	event []byte // lines of the event being read
	out   []byte // bytes ready for the caller
	pass  bool   // an oversized line was seen; pass the event on unexamined
	err   error
}

func (d *dropper) Read(p []byte) (int, error) {
	for len(d.out) == 0 && d.err == nil {
		line, err := d.br.ReadSlice('\n')
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			d.out = append(append(d.out, d.event...), line...)
			d.event, d.pass = d.event[:0], true
			continue
		case err != nil:
			d.event = append(d.event, line...)
			d.out, d.event, d.err = append(d.out, d.event...), d.event[:0], err
			continue
		}
		if d.pass {
			d.out = append(d.out, line...)
			d.pass = !isBlank(line)
			continue
		}
		d.event = append(d.event, line...)
		if isBlank(line) {
			if !d.drop(eventData(d.event)) {
				d.out = append(d.out, d.event...)
			}
			d.event = d.event[:0]
		}
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	if len(d.out) > 0 {
		return n, nil
	}
	return n, d.err
}

func (d *dropper) Close() error { return d.c.Close() }

func isBlank(line []byte) bool { return len(bytes.TrimRight(line, "\r\n")) == 0 }

// eventData returns the payload of an event's first data line.
func eventData(event []byte) []byte {
	for line := range bytes.Lines(event) {
		if data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:")); ok {
			return bytes.TrimPrefix(data, []byte(" "))
		}
	}
	return nil
}
