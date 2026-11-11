// Package id generates prefixed, time-sortable identifiers.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// Crockford base32, as used by ULIDs: sortable and unambiguous.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu   sync.Mutex
	last [16]byte
)

// New returns prefix + "_" + a 26 character ULID-style string: 48 bits
// of millisecond time followed by 80 random bits. Ids made in the same
// millisecond by one process increase, as monotonic ULIDs do, so lists
// ordered by id keep the order things happened in.
func New(prefix string) string {
	ms := uint64(time.Now().UnixMilli())
	mu.Lock()
	b := last
	if lastMS := binary.BigEndian.Uint64(last[:8]) >> 16; ms > lastMS {
		binary.BigEndian.PutUint64(b[:8], ms<<16)
		_, _ = rand.Read(b[6:])
	} else {
		// Same millisecond, or the clock went back: the last id plus one.
		for i := 15; i >= 6; i-- {
			b[i]++
			if b[i] != 0 {
				break
			}
		}
	}
	last = b
	mu.Unlock()
	return prefix + "_" + encode(b)
}

func encode(b [16]byte) string {
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	var out [26]byte
	// 128 bits in 26 base32 digits: the first digit carries 3 bits.
	for i := 25; i >= 0; i-- {
		out[i] = alphabet[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}
