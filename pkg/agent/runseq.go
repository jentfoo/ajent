package agent

import (
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jentfoo/ajent/pkg/llm"
)

// RunSequencer numbers the passes of one injected-call producer (@ expansion, a
// /init survey) so its call ids stay unique for the session and recoverable on
// resume. The id prefix is the producer's package-local string, passed at
// construction, keeping each transcript format with its owner.
type RunSequencer struct {
	prefix string
	run    atomic.Int64 // highest minted run; Seed raises it only
}

// NewRunSequencer returns a sequencer whose call ids lead with prefix, e.g.
// "ref-" or "init-".
func NewRunSequencer(prefix string) *RunSequencer {
	return &RunSequencer{prefix: prefix}
}

// Next mints the next run number. One pass takes one value and threads it into
// every CallID that pass makes.
func (s *RunSequencer) Next() int64 { return s.run.Add(1) }

// Seed raises the counter to exactly the highest run already carried by a
// prefixed call id in msgs, never past it: the +1 belongs to the caller's next
// Next. Seeding max+1 would mint a skipped number for the first id after resume.
func (s *RunSequencer) Seed(msgs []llm.Message) {
	var high int64
	for _, m := range msgs {
		for _, blk := range m.Content {
			call, ok := blk.(llm.ToolCallBlock)
			if !ok {
				continue
			}
			if n := s.runOf(call.ID); n > high {
				high = n
			}
		}
	}
	for { // raise-only CAS: a concurrent Next or Seed never drags it back down
		cur := s.run.Load()
		if cur >= high || s.run.CompareAndSwap(cur, high) {
			return
		}
	}
}

// CallID names one injected call for a run: prefix, run number, then rest.
func (s *RunSequencer) CallID(run int64, rest string) string {
	return s.prefix + strconv.FormatInt(run, 10) + "-" + rest
}

// runOf returns the run number encoded in a CallID id, or 0 for foreign or
// malformed ids.
func (s *RunSequencer) runOf(id string) int64 {
	rest, ok := strings.CutPrefix(id, s.prefix)
	if !ok {
		return 0
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
