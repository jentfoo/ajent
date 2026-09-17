package app

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hintRecorder records what a board paints, safe for concurrent use.
type hintRecorder struct {
	mu     sync.Mutex
	texts  []string
	shorts []string
}

func (r *hintRecorder) record(text, short string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.texts = append(r.texts, text)
	r.shorts = append(r.shorts, short)
}

// last returns the most recently painted text as a one-element slice, or nil.
func (r *hintRecorder) last() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.texts) == 0 {
		return nil
	}
	return []string{r.texts[len(r.texts)-1]}
}

func TestHintBoardRequest(t *testing.T) {
	t.Parallel()

	t.Run("newest_claim_wins", func(t *testing.T) {
		rec := &hintRecorder{}
		b := newHintBoard(rec.record)

		first := b.Request("paused 9s", "9s")
		require.NotNil(t, first)
		assert.Equal(t, []string{"paused 9s"}, rec.last())

		b.Request("ctrl+c again to quit", "")
		assert.Equal(t, []string{"ctrl+c again to quit"}, rec.last())
	})

	t.Run("freeing_restores_older_claim", func(t *testing.T) {
		rec := &hintRecorder{}
		b := newHintBoard(rec.record)

		paused := b.Request("paused 9s", "9s")
		arm := b.Request("ctrl+c again to quit", "")

		arm.Free()
		assert.Equal(t, []string{"paused 9s"}, rec.last()) // the hold reclaims its line

		paused.Set("paused 4s", "4s")
		assert.Equal(t, []string{"paused 4s"}, rec.last())
	})

	t.Run("masked_slot_keeps_latest_text", func(t *testing.T) {
		rec := &hintRecorder{}
		b := newHintBoard(rec.record)

		paused := b.Request("paused 9s", "9s")
		quitting := b.Request("quitting…", "") // masks the older claim
		before := len(rec.texts)
		paused.Set("paused 5s", "5s") // masked: stored, not painted
		assert.Len(t, rec.texts, before)

		quitting.Free()
		assert.Equal(t, []string{"paused 5s"}, rec.last())
	})

	t.Run("clear_paints_once", func(t *testing.T) {
		rec := &hintRecorder{}
		b := newHintBoard(rec.record)

		s := b.Request("hi", "")
		s.Free()
		require.Equal(t, []string{""}, rec.last())

		s.Free() // idempotent: no second clear write
		assert.Len(t, rec.texts, 2)
	})

	t.Run("free_slot_ignores_late_sets", func(t *testing.T) {
		rec := &hintRecorder{}
		b := newHintBoard(rec.record)

		s := b.Request("hi", "")
		s.Free()
		n := len(rec.texts)
		s.Set("stale", "")
		assert.Len(t, rec.texts, n)
	})

	t.Run("empty_text_requests_nothing", func(t *testing.T) {
		rec := &hintRecorder{}
		b := newHintBoard(rec.record)

		assert.Nil(t, b.Request("", ""))
		assert.Empty(t, rec.texts)
		var nilBoard *hintBoard
		assert.Nil(t, nilBoard.Request("x", ""))
	})
}

func TestHintBoardShowFor(t *testing.T) {
	t.Parallel()

	rec := &hintRecorder{}
	b := newHintBoard(rec.record)
	paused := b.Request("paused 9s", "9s")

	b.ShowFor("cancelled shell command", "", 20*time.Millisecond)
	require.Equal(t, []string{"cancelled shell command"}, rec.last())

	// the notice expires on its own and the older claim wins the line again; a Set from
	// that holder then paints, proving it still owns a live slot
	require.Eventually(t, func() bool { return rec.last()[0] == "paused 9s" }, time.Second, time.Millisecond,
		"an expired notice must release the line to the older claim")
	paused.Set("paused 2s", "2s")
	assert.Equal(t, []string{"paused 2s"}, rec.last())
}

func TestHintLine(t *testing.T) {
	t.Parallel()

	rec := &hintRecorder{}
	b := newHintBoard(rec.record)
	line := b.Line()

	line.Set("paused 9s", "9s") // takes the claim on first use
	line.Set("paused 7s", "7s") // refreshes it without jumping the queue
	require.Equal(t, []string{"paused 7s"}, rec.last())

	other := b.Request("quitting…", "")
	line.Set("paused 3s", "3s") // stays stored while masked
	assert.Equal(t, []string{"quitting…"}, rec.last())

	other.Free()
	assert.Equal(t, []string{"paused 3s"}, rec.last())

	line.Set("", "") // releases
	assert.Equal(t, []string{""}, rec.last())
	line.Set("after release", "")
	assert.Equal(t, []string{"after release"}, rec.last())

	var nilLine *hintLine
	nilLine.Set("x", "") // no board, no panic
}
