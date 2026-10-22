// Package id generates prefixed, time-sortable identifiers.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// Crockford base32, as used by ULIDs: sortable and unambiguous.
const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns prefix + "_" + a 26 character ULID-style string: 48 bits
// of millisecond time followed by 80 random bits.
func New(prefix string) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], uint64(time.Now().UnixMilli())<<16)
	_, _ = rand.Read(b[6:])
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
