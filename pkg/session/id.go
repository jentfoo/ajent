package session

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

// crockford is the base32 alphabet ULID uses, minus I L O U.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// idTailBits sizes an id's random tail. 12 Crockford chars pack 60 bits:
// 32-bit unix seconds (to 2106) above this tail.
const idTailBits = 28

// clock supplies timestamps so tests are deterministic.
var clock = func() time.Time { return time.Now().UTC() }

var (
	mu     sync.Mutex
	lastMS int64  // held-forward millisecond clock feeding ids and entry stamps
	idSec  uint64 // second idTail counts within, from lastMS
	idTail uint32 // random-seeded counter, re-seeded per advancing second
)

// monoMilli returns a monotonic millisecond stamp: an advancing clock is
// adopted fresh, otherwise the last value holds. Ids and entry stamps share it,
// so ordering by either always agrees with append order.
func monoMilli() int64 {
	mu.Lock()
	defer mu.Unlock()
	return holdMSLocked()
}

// holdMSLocked advances lastMS to the clock when possible. Caller holds mu.
func holdMSLocked() int64 {
	if ms := clock().UnixMilli(); ms > lastMS {
		lastMS = ms
	}
	return lastMS
}

// NewID returns a 12-char Crockford id packing 60 bits as 32-bit unix seconds
// plus a 28-bit random tail, lexically ordered by creation time to the second.
// Same-second ids across processes collide with probability at most 2^-28 per
// pair; within one process they are strictly increasing.
func NewID() string {
	mu.Lock()
	defer mu.Unlock()

	sec := uint64(holdMSLocked()) / 1000
	if sec != idSec { // fresh second: re-seed entropy so tails spread across real time
		idSec = sec
		var b [4]byte
		_, _ = rand.Read(b[:]) // entropy failure leaves zero, still monotonic per process
		idTail = binary.BigEndian.Uint32(b[:]) >> (32 - idTailBits)
	}
	idTail++                    // strictly increasing even when the clock is pinned or ticks backward
	idTail &= 1<<idTailBits - 1 // wrap at 2^28 ids in one second is unreachable; mask documents width

	v := sec<<idTailBits | uint64(idTail)
	var out [12]byte
	for i := range out {
		out[i] = crockford[v>>(5*(11-i))&0x1f]
	}
	return string(out[:])
}
