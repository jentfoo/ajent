package agent

import (
	"strconv"
	"sync"
	"testing"

	"github.com/go-analyze/bulk"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
)

// callMsgs wraps each id in its own message, the shape Seed scans.
func callMsgs(ids ...string) []llm.Message {
	msgs := make([]llm.Message, len(ids))
	for i, id := range ids {
		msgs[i] = llm.Message{Content: llm.BlockList{llm.ToolCallBlock{ID: id}}}
	}
	return msgs
}

func TestNext(t *testing.T) {
	t.Parallel()

	s := NewRunSequencer("t-")
	assert.Equal(t, int64(1), s.Next())
	assert.Equal(t, int64(2), s.Next())
}

func TestSeed(t *testing.T) {
	t.Parallel()

	// Seed lifts the counter to exactly the highest run already in context, so a
	// later Next continues rather than skips or repeats the recorded ids
	t.Run("raises_to_max_not_past", func(t *testing.T) {
		s := NewRunSequencer("t-")
		s.Seed(callMsgs("t-3-read-1", "call_9", "t-7-poll-x-2"))
		assert.Equal(t, int64(7), s.run.Load())
		assert.Equal(t, int64(8), s.Next())
	})

	t.Run("never_lowers", func(t *testing.T) {
		s := NewRunSequencer("t-")
		s.run.Store(10)
		s.Seed(callMsgs("t-9-read-1"))
		assert.Equal(t, int64(10), s.run.Load())
	})

	t.Run("ignores_foreign_and_malformed", func(t *testing.T) {
		s := NewRunSequencer("t-")
		s.Seed(callMsgs("", "ref-4-a", "t-", "t-x-read-1", "t--5-x", "t-9"))
		assert.Equal(t, int64(0), s.run.Load())
	})

	t.Run("empty_transcript_keeps_zero", func(t *testing.T) {
		s := NewRunSequencer("t-")
		s.Seed(nil)
		assert.Equal(t, int64(0), s.run.Load())
	})

	// Seed racing Next must only ever raise: the CAS keeps a seed from dragging
	// freshly minted ids back into reuse
	t.Run("concurrent_with_next", func(t *testing.T) {
		s := NewRunSequencer("t-")
		const workers, per = 4, 50
		nexts := make([]int64, workers*per)
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < per; i++ {
					s.Seed(callMsgs("t-" + strconv.Itoa(i) + "-x"))
					nexts[w*per+i] = s.Next() // distinct slots, no sharing
				}
			}(w)
		}
		wg.Wait()

		assert.Len(t, bulk.SliceToSet(nexts), workers*per) // a lowered counter would mint duplicates
		assert.GreaterOrEqual(t, s.run.Load(), int64(per-1))
	})
}

func TestCallID(t *testing.T) {
	t.Parallel()

	s := NewRunSequencer("t-")
	assert.Equal(t, "t-42-r-7-x", s.CallID(42, "r-7-x"))

	// a digits-led rest must not read as a second run number: parsing stops at
	// the first dash after the run
	t.Run("run_survives_digit_led_rest", func(t *testing.T) {
		require.Equal(t, int64(42), s.runOf(s.CallID(42, "7-x")))
	})
}
