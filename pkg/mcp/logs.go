package mcp

import "sync"

// ringLog is a bounded, thread-safe buffer of recent server stderr and protocol
// lines for /mcp logs. No logging framework in the repo.
type ringLog struct {
	mu  sync.Mutex
	buf []string
	max int
}

func newRingLog(max int) *ringLog { return &ringLog{max: max} }

// add appends a line, dropping the oldest once at capacity.
func (l *ringLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == l.max {
		copy(l.buf, l.buf[1:])
		l.buf[len(l.buf)-1] = line
		return
	}
	l.buf = append(l.buf, line)
}

func (l *ringLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.buf))
	copy(out, l.buf)
	return out
}
