package subagent

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJob(t *testing.T) {
	t.Parallel()

	t.Run("completes", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: summaryTurn("found it at pkg/a.go:12", llm.Usage{Input: 100, Output: 20})},
		})
		m := New(Options{
			Provider: p,
			Model:    func() llm.Model { return llm.Model{ID: "child", ContextWindow: 8000} },
			Tools: &fakeSource{tools: []agent.Tool{
				&fakeTool{name: "read"}, roTool("grep")},
			},
		})
		t.Cleanup(m.Close)

		id := m.Start("find the bug", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		assert.Contains(t, j.Summary, "pkg/a.go:12")
	})

	t.Run("errors", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{{Err: errors.New("provider exploded")}})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		id := m.Start("boom", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusError, j.Status)
		require.Error(t, j.Err)
	})

	t.Run("aborted_by_stop", func(t *testing.T) {
		m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return &blockingProvider{}, nil }})
		t.Cleanup(m.Close)

		id := m.Start("long", "")
		require.NoError(t, m.Stop(id))
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusAborted, j.Status)
	})
}

func TestCompletionNotification(t *testing.T) {
	t.Parallel()

	// a finished job reaches the user and the model when nobody is polling for it.
	t.Run("notifies_and_steers_without_poll", func(t *testing.T) {
		c := newCapture()
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Notice:   func(s string) { c.mu.Lock(); c.notices = append(c.notices, s); c.mu.Unlock() },
			Deliver: func(in agent.Input) bool {
				c.mu.Lock()
				c.delivers = append(c.delivers, in)
				c.mu.Unlock()
				return true // parent running; the steer lands
			},
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		require.Eventually(t, func() bool {
			s, ok := jobStatus(m, id)
			return ok && s == StatusDone
		}, time.Second, 5*time.Millisecond)

		assert.Equal(t, 1, c.noticeCount())
		// a steer naming the completed id was offered to the running parent
		txts := c.deliveredTexts()
		require.NotEmpty(t, txts)
		assert.Contains(t, txts[len(txts)-1], id)
	})

	t.Run("suppressed_when_polling", func(t *testing.T) {
		c := newCapture()
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("summary", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Notice:   func(s string) { c.mu.Lock(); c.notices = append(c.notices, s); c.mu.Unlock() },
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		// a poll held the wait for this job and consumed it, so no completion notice was sent
		assert.Zero(t, c.noticeCount())
	})

	t.Run("deliver_idle_leaves_pending_and_flush_reoffers", func(t *testing.T) {
		c := newCapture()
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s1", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Deliver: func(in agent.Input) bool { // parent idle; ids stay pending
				c.mu.Lock()
				c.delivers = append(c.delivers, in)
				c.mu.Unlock()
				return false
			},
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		require.Eventually(t, func() bool {
			s, ok := jobStatus(m, id)
			return ok && s == StatusDone
		}, time.Second, 5*time.Millisecond)
		// the completion was offered once and left pending because the parent is idle
		assert.Len(t, c.deliveredTexts(), 1)

		m.Flush() // a turn-start observer re-offers pending completions
		require.Eventually(t, func() bool { return len(c.deliveredTexts()) == 2 }, time.Second, 5*time.Millisecond)
	})

	t.Run("delivered_clears_only_named_ids", func(t *testing.T) {
		var mu sync.Mutex // offer runs from both the spawn completion and this test
		var delivered []agent.Input
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("one", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Deliver: func(in agent.Input) bool {
				mu.Lock()
				delivered = append(delivered, in)
				mu.Unlock()
				return true
			},
		})
		t.Cleanup(m.Close)

		id1 := m.Start("a", "")
		m.mu.Lock()
		m.pending = []string{"sub-9", id1} // a second completion already queued but undelivered
		m.mu.Unlock()

		m.offer([]string{id1}) // delivers naming only id1; its confirm clears just that id
		mu.Lock()
		require.Len(t, delivered, 1)
		in := delivered[0]
		mu.Unlock()
		in.Delivered() // simulate the steer landing in context

		assert.Equal(t, []string{"sub-9"}, m.pending, "only the named id is cleared")
	})

	t.Run("idle_completion_never_starts_turn", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Deliver:  func(in agent.Input) bool { return false }, // parent idle
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
		m.Flush() // still no deliverer for an idle agent; nothing must start a turn
	})
}
func TestPollTimeoutThenComplete(t *testing.T) {
	t.Parallel()
	d := &delayedProvider{turn: summaryTurn("slow but done", llm.Usage{}), release: make(chan struct{})}
	m := New(Options{
		Provider:    func(llm.Model) (llm.Provider, error) { return d, nil },
		PollTimeout: 30 * time.Millisecond,
	})
	t.Cleanup(m.Close)

	id := m.Start("slow", "")
	j1, ok := m.Poll(t.Context(), id)
	assert.False(t, ok, "should report still running on timeout")
	assert.Equal(t, StatusRunning, j1.Status)

	m.mu.Lock()
	var prog string
	if jj := m.jobs[normalizeID(id)]; jj != nil {
		prog = jj.pollProgress()
	}
	m.mu.Unlock()
	assert.Contains(t, prog, "still running after")

	close(d.release) // let the turn finish
	j2, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusDone, j2.Status)
	assert.Contains(t, j2.Summary, "slow but done")
}

func TestConcurrencyBoundedBySemaphore(t *testing.T) {
	t.Parallel()
	const total, max = 8, 4
	g := &gatedProvider{}
	m := New(Options{
		Provider:      func(llm.Model) (llm.Provider, error) { return g, nil },
		MaxConcurrent: max,
	})
	t.Cleanup(m.Close)

	var ids []string
	for i := 0; i < total; i++ {
		ids = append(ids, m.Start("job "+strconv.Itoa(i), ""))
	}
	// exactly max run at once proves the semaphore holds the rest queued
	require.Eventually(t, func() bool { return g.active.Load() == int32(max) }, time.Second, 5*time.Millisecond)

	g.releaseAll()
	for _, id := range ids {
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusDone, j.Status)
	}
	assert.LessOrEqual(t, g.peak.Load(), int32(max), "never more than max run at once")
}

func TestShutdownCancelsRunningJobs(t *testing.T) {
	t.Parallel()
	b := &blockingProvider{}
	c := newCapture()
	m := New(Options{
		Provider: func(llm.Model) (llm.Provider, error) { return b, nil },
		Activity: c.recordRow,
	})
	id := m.Start("long", "")
	m.Close() // must cancel and return promptly

	j, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusAborted, j.Status)

	// the activity row was cleared on close (empty text for the key)
	require.Eventually(t, func() bool { return c.rowText(id) == "" }, time.Second, 5*time.Millisecond)
}

// TestActivityRow covers how a job's row appears and is cleared.
func TestActivityRow(t *testing.T) {
	t.Parallel()

	// a job is visible above the prompt as soon as it starts (even before its turn
	// emits), and every terminal path clears the row.
	t.Run("start_publishes", func(t *testing.T) {
		g := &gatedProvider{}
		c := newCapture()
		m := New(Options{
			Provider: func(llm.Model) (llm.Provider, error) { return g, nil },
			Activity: c.recordRow,
		})
		t.Cleanup(m.Close)

		id := m.Start("one", "")
		// the row appears immediately while the job is queued/running and pinned open.
		assert.Equal(t, "sub-1  one", c.rowText(id))

		g.releaseAll()
		m.Poll(t.Context(), id)
		require.Eventually(t, func() bool { return c.rowText(id) == "" }, time.Second, 5*time.Millisecond)
	})

	// a job cancelled before acquiring its slot still clears the row Start published (no childSink ever ran).
	t.Run("queued_cancelled_clears", func(t *testing.T) {
		g := &blockingProvider{}
		c := newCapture()
		m := New(Options{
			Provider: func(llm.Model) (llm.Provider, error) { return g, nil },
			Activity: c.recordRow,
		})
		t.Cleanup(m.Close)

		m.Start("one", "")
		m.Start("two", "") // queued behind the blocking first job
		// both jobs show a row: sub-1 is running/pinned open, sub-2 waits on the slot.
		assert.Equal(t, "sub-1  one", c.rowText("sub-1"))
		assert.Equal(t, "sub-2  two", c.rowText("sub-2"))

		// cancel the queued second job; its row must still be cleared even though it never ran.
		require.NoError(t, m.Stop("sub-2"))
		require.Eventually(t, func() bool { return c.rowText("sub-2") == "" }, time.Second, 5*time.Millisecond)
	})
}

func TestStatusSegmentAndList(t *testing.T) {
	t.Parallel()
	g := &gatedProvider{}
	var mu sync.Mutex // publishStatus runs from concurrent job goroutines
	var statuses []string
	m := New(Options{
		Provider:      func(llm.Model) (llm.Provider, error) { return g, nil },
		MaxConcurrent: 2,
		Status: func(text, short string) {
			if text == "" {
				return
			}
			mu.Lock()
			statuses = append(statuses, text)
			mu.Unlock()
		},
	})
	t.Cleanup(m.Close)

	id1 := m.Start("one", "")
	m.Start("two", "")
	g.releaseAll()
	m.Poll(t.Context(), id1)

	jobs := m.List()
	assert.Len(t, jobs, 2)
	assert.Contains(t, jobs[0].ID, "sub-")
	// a status was published with the running count
	mu.Lock()
	found := false
	for _, s := range statuses {
		if strings.Contains(s, "running") {
			found = true
		}
	}
	mu.Unlock()
	assert.True(t, found)
}

func TestStopAllCancelsEverything(t *testing.T) {
	t.Parallel()
	b := &blockingProvider{}
	m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return b, nil }})
	t.Cleanup(m.Close)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, m.Start("x", ""))
	}
	n := m.StopAll()
	assert.Equal(t, 3, n)
	for _, id := range ids {
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		assert.Equal(t, StatusAborted, j.Status)
	}
}

// TestChildSpendRollsIntoParentLedger verifies a child's usage appears as child
// spend on the parent ledger without moving its context.
func TestChildSpendRollsIntoParentLedger(t *testing.T) {
	t.Parallel()
	parent := tokens.New(llm.Model{ID: "parent", ContextWindow: 8000})
	p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{Input: 200, Output: 40})}})
	m := New(Options{
		Provider: p,
		Parent:   func() *tokens.Accounting { return parent },
	})
	t.Cleanup(m.Close)

	id := m.Start("q", "")
	j, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, StatusDone, j.Status)

	require.Eventually(t, func() bool { return !tokens.Zero(parent.ChildTotal()) }, time.Second, 5*time.Millisecond)
	child := parent.ChildTotal()
	assert.Equal(t, 200, child.Input)
	assert.Equal(t, 40, child.Output)
	// the delegated subset is also part of total
	total := parent.Total()
	assert.GreaterOrEqual(t, total.Input, child.Input)
}

// TestParentContextUnchangedByChild verifies a running child does not move the
// parent's context bar.
func TestParentContextUnchangedByChild(t *testing.T) {
	t.Parallel()
	parent := tokens.New(llm.Model{ID: "parent", ContextWindow: 8000})
	g := &gatedProvider{}
	m := New(Options{
		Provider:      func(llm.Model) (llm.Provider, error) { return g, nil },
		Parent:        func() *tokens.Accounting { return parent },
		MaxConcurrent: 1,
	})
	t.Cleanup(m.Close)

	id := m.Start("q", "")
	require.Eventually(t, func() bool { return g.active.Load() == 1 }, time.Second, 5*time.Millisecond) // child streaming
	before := parent.Context().Used
	g.releaseAll()
	_, ok := m.Poll(t.Context(), id)
	require.True(t, ok)
	assert.Equal(t, before, parent.Context().Used, "a child's run must not move the parent context bar")
}

// jobStatus returns id's live status, with ok=false when unknown.
func jobStatus(m *Manager, id string) (Status, bool) {
	j, ok := m.lookup(id)
	if !ok {
		return 0, false
	}
	return j.statusOf(), true
}
