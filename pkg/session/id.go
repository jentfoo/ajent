package session

import (
	"crypto/rand"
	"sync"
	"time"
)

// crockford is the base32 alphabet ULID uses, minus I L O U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// clock supplies timestamps so tests are deterministic.
var clock = func() time.Time { return time.Now().UTC() }

var (
	mu       sync.Mutex
	lastMS   int64
	lastRand [10]byte // monotonic within a millisecond by incrementing this
)

// NewID returns a 26-char Crockford ULID: a 48-bit millisecond timestamp plus
// 80 random bits, lexically sortable by creation time.
func NewID() string {
	var b [16]byte
	ms := clock().UnixMilli()
	for i := range 6 {
		b[i] = byte(uint64(ms) >> uint((5-i)*8))
	}

	mu.Lock()
	if ms == lastMS {
		incrementRand(lastRand[:])
	} else {
		lastMS = ms
		_, _ = rand.Read(lastRand[:]) // entropy failure leaves zero, still monotonic per process
	}
	copy(b[6:16], lastRand[:])
	out := encodeULID(b)
	mu.Unlock()
	return out
}

// incrementRand carries a big-endian counter forward by one.
func incrementRand(r []byte) {
	for i := len(r) - 1; i >= 0; i-- {
		r[i]++
		if r[i] != 0 {
			break
		}
	}
}

// encodeULID renders a 128-bit id as Crockford chars, MSB first.
func encodeULID(id [16]byte) string {
	var out [26]byte
	for i := range out {
		out[i] = crockford[readBits5(id[:], i*5)]
	}
	return string(out[:])
}

// readBits5 returns the five bits of b starting at start, MSB first.
func readBits5(b []byte, start int) byte {
	var v uint32
	for k := range 5 { // bits at start..start+4, MSB first
		if bitAt(b, start+k) {
			v |= 1 << (4 - k)
		}
	}
	return byte(v)
}

// bitAt reports whether the bit at index i is set.
func bitAt(b []byte, i int) bool {
	if i >= len(b)*8 {
		return false
	}
	return b[i/8]&(0x80>>uint(i%8)) != 0
}
