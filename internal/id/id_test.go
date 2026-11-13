package id

import (
	"strings"
	"testing"
	"time"
)

func TestNewIsPrefixedUniqueAndSortable(t *testing.T) {
	a := New("req")
	time.Sleep(2 * time.Millisecond)
	b := New("req")
	if !strings.HasPrefix(a, "req_") || len(a) != len("req_")+26 {
		t.Fatalf("bad id %q", a)
	}
	if a == b || a >= b {
		t.Fatalf("ids not unique and increasing: %s %s", a, b)
	}
}

func TestNewIncreasesWithinAMillisecond(t *testing.T) {
	prev := New("x")
	for range 10000 {
		next := New("x")
		if next <= prev {
			t.Fatalf("%s after %s", next, prev)
		}
		prev = next
	}
}
