package subagent

import (
	"encoding/json"
	"errors"
	"slices"
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
	t.Run("notifies_at_completion_steers_at_boundary", func(t *testing.T) {
		c := newCapture()
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Notice:   func(s string) { c.mu.Lock(); c.notices = append(c.notices, s); c.mu.Unlock() },
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		require.Eventually(t, func() bool { return c.noticeCount() == 1 }, 2*time.Second, 5*time.Millisecond)
		assert.Equal(t, []string{"Sub-agent " + id + " completed"}, c.noticeTexts())

		ins := m.Boundary() // the step boundary pulls the batched completion input
		require.Len(t, ins, 1)
		assert.Contains(t, ins[0].Text, id)
		ins[0].Delivered() // simulate the message landing
		m.mu.Lock()
		assert.Empty(t, m.pending)
		m.mu.Unlock()
	})

	t.Run("suppressed_when_polling", func(t *testing.T) {
		c := newCapture()
		g := &gatedProvider{} // completion held until the poller is registered
		m := New(Options{
			Provider: func(llm.Model) (llm.Provider, error) { return g, nil },
			Notice:   func(s string) { c.mu.Lock(); c.notices = append(c.notices, s); c.mu.Unlock() },
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		type pollRes struct {
			j  Job
			ok bool
		}
		res := make(chan pollRes, 1)
		go func() { // register the poller before the job can finish
			j, ok := m.Poll(t.Context(), id)
			res <- pollRes{j, ok}
		}()
		require.Eventually(t, func() bool { // the poller must be waiting first
			jj, ok := m.lookup(id)
			return ok && jj.statusOf() == StatusRunning
		}, 2*time.Second, 5*time.Millisecond)

		g.releaseAll()
		r := <-res
		require.True(t, r.ok)
		assert.Equal(t, StatusDone, r.j.Status)
		// a poll held the wait for this job and consumed it, so no notice or steer names it
		assert.Zero(t, c.noticeCount())
		assert.Empty(t, m.Boundary())
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
		// completion alone offers nothing while the parent is idle: no timer exists
		// to fire, so the ids wait for the next turn start
		require.Eventually(t, func() bool {
			s, ok := jobStatus(m, id)
			return ok && s == StatusDone
		}, 2*time.Second, 5*time.Millisecond)
		assert.Empty(t, c.deliveredTexts())

		m.Flush() // a turn-start observer offers pending completions
		require.Eventually(t, func() bool { return len(c.deliveredTexts()) == 1 }, 2*time.Second, 5*time.Millisecond)

		m.Flush() // still undelivered (idle); the next turn start offers again
		require.Eventually(t, func() bool { return len(c.deliveredTexts()) == 2 }, 2*time.Second, 5*time.Millisecond)
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

		assert.Equal(t, []string{"sub-9"}, m.pending)
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

// TestCompletionBatching covers the one-message-per-boundary contract: completions
// share a single keyed notice and a single steer, ids a poll already claimed are
// never named, and an interrupt releases the in-flight marks for re-offer.
func TestCompletionBatching(t *testing.T) {
	t.Parallel()

	// completions between boundaries ride one input naming every id
	t.Run("boundary_merges_batch", func(t *testing.T) {
		c := newCapture()
		g := &gatedProvider{}
		m := New(Options{
			Provider: func(llm.Model) (llm.Provider, error) { return g, nil },
			Notice:   func(s string) { c.mu.Lock(); c.notices = append(c.notices, s); c.mu.Unlock() },
		})
		t.Cleanup(m.Close)

		ids := []string{m.Start("a", ""), m.Start("b", ""), m.Start("c", "")}
		g.releaseAll()
		require.Eventually(t, func() bool { return c.noticeCount() == 3 }, 2*time.Second, 5*time.Millisecond)
		// the keyed notice accumulates: the last one names every completion
		assert.Equal(t, "Sub-agents "+strings.Join(ids, ", ")+" completed", c.lastNotice())

		ins := m.Boundary()
		require.Len(t, ins, 1)
		assert.Contains(t, ins[0].Text, strings.Join(ids, ", "))
		// ids are now in flight; a second boundary pull sends nothing (per-id marks)
		assert.Empty(t, m.Boundary())
	})

	// the observed failure mode: a batch that queued mid-step must not name ids
	// the model polled before the message landed
	t.Run("boundary_drops_polled_ids", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: summaryTurn("one", llm.Usage{})},
			{Events: summaryTurn("two", llm.Usage{})},
		})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		id1 := m.Start("a", "")
		id2 := m.Start("b", "")
		for _, id := range []string{id1, id2} {
			require.Eventually(t, func() bool {
				s, ok := jobStatus(m, id)
				return ok && s == StatusDone
			}, 2*time.Second, 5*time.Millisecond)
		}

		_, ok := m.Poll(t.Context(), id1) // the model polls one result itself
		require.True(t, ok)

		ins := m.Boundary()
		require.Len(t, ins, 1)
		assert.Contains(t, ins[0].Text, id2)
		assert.NotContains(t, ins[0].Text, id1, "a polled id must never be named")
	})

	// every result polled before the boundary: no input at all
	t.Run("boundary_silent_when_all_polled", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: summaryTurn("one", llm.Usage{})},
			{Events: summaryTurn("two", llm.Usage{})},
		})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		ids := []string{m.Start("a", ""), m.Start("b", "")}
		for _, id := range ids {
			j, ok := m.Poll(t.Context(), id)
			require.True(t, ok)
			assert.Equal(t, StatusDone, j.Status)
		}
		assert.Empty(t, m.Boundary())
	})

	// an interrupt drops a queued steer without its Delivered; the marks must
	// release so the next turn start can re-offer the same ids
	t.Run("interrupt_reoffers_dropped_batch", func(t *testing.T) {
		c := newCapture()
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
		m := New(Options{
			Provider: p,
			Deliver: func(in agent.Input) bool {
				c.mu.Lock()
				c.delivers = append(c.delivers, in)
				c.mu.Unlock()
				return true
			},
		})
		t.Cleanup(m.Close)

		id := m.Start("x", "")
		require.Eventually(t, func() bool {
			s, ok := jobStatus(m, id)
			return ok && s == StatusDone
		}, 2*time.Second, 5*time.Millisecond)
		m.Flush() // the turn start queued the batch into the interrupted turn
		require.Len(t, c.deliveredTexts(), 1)

		m.Interrupted() // queued steer dropped; Delivered will never fire
		m.Flush()       // the next turn start re-offers the pending batch
		txts := c.deliveredTexts()
		require.Len(t, txts, 2)
		assert.Contains(t, txts[1], id)
	})

	// a poll claiming an id must also clear its notice-batch entry so the next
	// completion's keyed notice does not re-name work the model already retrieved
	t.Run("poll_clears_notice_batch", func(t *testing.T) {
		c := newCapture()
		g := &gatedProvider{}
		m := New(Options{
			Provider: func(llm.Model) (llm.Provider, error) { return g, nil },
			Notice:   func(s string) { c.mu.Lock(); c.notices = append(c.notices, s); c.mu.Unlock() },
		})
		t.Cleanup(m.Close)

		id1 := m.Start("a", "")
		g.releaseAll()
		require.Eventually(t, func() bool { return c.noticeCount() == 1 }, 2*time.Second, 5*time.Millisecond)
		assert.Equal(t, "Sub-agent "+id1+" completed", c.lastNotice())

		_, ok := m.Poll(t.Context(), id1) // the model retrieves the result itself
		require.True(t, ok)

		id2 := m.Start("b", "")
		g.releaseAll()
		require.Eventually(t, func() bool { return c.noticeCount() == 2 }, 2*time.Second, 5*time.Millisecond)
		assert.Equal(t, "Sub-agent "+id2+" completed", c.lastNotice())
	})

	// a leaked in-flight mark for an id that no longer exists must not stall the
	// boundary: take skips per-id marks, so unrelated completions still ride it
	t.Run("stranded_mark_never_stalls_boundary", func(t *testing.T) {
		p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("s", llm.Usage{})}})
		m := New(Options{Provider: p})
		t.Cleanup(m.Close)

		m.mu.Lock()
		m.inFlight = []string{"sub-9"} // a mark for a job that no longer exists
		m.mu.Unlock()

		id := m.Start("x", "")
		require.Eventually(t, func() bool {
			s, ok := jobStatus(m, id)
			return ok && s == StatusDone
		}, 2*time.Second, 5*time.Millisecond)

		ins := m.Boundary()
		require.Len(t, ins, 1)
		assert.Contains(t, ins[0].Text, id)
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
	assert.False(t, ok)
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

// TestReserve covers id reservation around the ordered batch: a reserved call
// claims its number whenever it runs, an unreserved start still gets one, and a
// new batch supersedes reservations whose call never ran.
func TestReserve(t *testing.T) {
	t.Parallel()

	t.Run("claims_in_batch_order", func(t *testing.T) {
		m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return &blockingProvider{}, nil }})
		t.Cleanup(m.Close)

		m.Reserve([]agent.ToolCall{
			{ID: "c1", Name: startToolName}, {ID: "c2", Name: startToolName}, {ID: "c3", Name: startToolName},
		})
		// claimed out of order, as the parallel goroutines would
		assert.Equal(t, "sub-3", m.start("c", "", "c3"))
		assert.Equal(t, "sub-1", m.start("a", "", "c1"))
		assert.Equal(t, "sub-2", m.start("b", "", "c2"))
	})

	t.Run("ignores_other_tools", func(t *testing.T) {
		m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return &blockingProvider{}, nil }})
		t.Cleanup(m.Close)

		m.Reserve([]agent.ToolCall{
			{ID: "r1", Name: "read"}, {ID: "c1", Name: startToolName}, {ID: "g1", Name: "grep"},
		})
		assert.Equal(t, "sub-1", m.start("a", "", "c1"))
	})

	// a host-driven start, or a call id the batch never named, still gets a number
	t.Run("unreserved_start_takes_next", func(t *testing.T) {
		m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return &blockingProvider{}, nil }})
		t.Cleanup(m.Close)

		m.Reserve([]agent.ToolCall{{ID: "c1", Name: startToolName}})
		assert.Equal(t, "sub-2", m.Start("host", ""))
		assert.Equal(t, "sub-1", m.start("a", "", "c1"))
		assert.Equal(t, "sub-3", m.start("stray", "", "unknown-call"))
	})

	// an interrupted turn leaves reservations nothing will claim; the next batch
	// drops them and their numbers are simply skipped
	t.Run("new_batch_supersedes", func(t *testing.T) {
		m := New(Options{Provider: func(llm.Model) (llm.Provider, error) { return &blockingProvider{}, nil }})
		t.Cleanup(m.Close)

		m.Reserve([]agent.ToolCall{{ID: "c1", Name: startToolName}, {ID: "c2", Name: startToolName}})
		m.Reserve([]agent.ToolCall{{ID: "c9", Name: startToolName}})
		assert.Equal(t, "sub-3", m.start("later", "", "c9"))
		assert.Equal(t, "sub-4", m.start("dropped", "", "c1"))
	})
}

// TestPollBatchDetection covers how a simultaneous poll group is spotted: every
// poll that overlapped another reports batched, including the one that arrived
// first, and the mark clears once the group empties so a later lone poll is bare.
func TestPollBatchDetection(t *testing.T) {
	t.Parallel()
	m := New(Options{})
	t.Cleanup(m.Close)

	m.enterPoll()
	assert.False(t, m.leavePoll())

	m.enterPoll() // a arrives
	m.enterPoll() // b overlaps it
	assert.True(t, m.leavePoll())
	assert.True(t, m.leavePoll())

	m.enterPoll() // the group emptied; the mark must not leak into the next poll
	assert.False(t, m.leavePoll())
}

// TestPollPrefersResultOverTimeout covers the select race: a job that completes in
// the same instant the poll times out must return its summary, never a
// still-running report the model would act on.
func TestPollPrefersResultOverTimeout(t *testing.T) {
	t.Parallel()
	p, _ := scripted([]llm.ScriptedTurn{{Events: summaryTurn("done in time", llm.Usage{})}})
	m := New(Options{Provider: p, PollTimeout: time.Nanosecond}) // the timer is always ready
	t.Cleanup(m.Close)

	id := m.Start("x", "")
	j, ok := m.lookup(id)
	require.True(t, ok)
	<-j.done // finished before the poll registers; both select cases are ready

	got, complete := m.Poll(t.Context(), id)
	require.True(t, complete)
	assert.Equal(t, StatusDone, got.Status)
	assert.Contains(t, got.Summary, "done in time")
}

// TestOrphanedCompletionRecovered covers a poll that departs empty-handed (its
// timer fired, or the turn was interrupted) in the same instant the job finished:
// onComplete saw pollers>0 and skipped the enqueue, so the last poller out has to
// re-arm delivery or the summary reaches nobody.
func TestOrphanedCompletionRecovered(t *testing.T) {
	t.Parallel()
	c := newCapture()
	release := make(chan struct{})
	m := New(Options{
		Provider: func(llm.Model) (llm.Provider, error) {
			return &delayedProvider{release: release, turn: summaryTurn("final", llm.Usage{})}, nil
		},
		Notice: func(msg string) { c.mu.Lock(); c.notices = append(c.notices, msg); c.mu.Unlock() },
	})
	t.Cleanup(m.Close)

	id := m.Start("x", "")
	j, ok := m.lookup(id)
	require.True(t, ok)

	j.mu.Lock() // a poll registered and about to leave on its timeout
	j.pollers++
	j.mu.Unlock()

	close(release)
	<-j.done // completes while the poller is still counted; onComplete stays silent
	assert.Zero(t, c.noticeCount())

	j.mu.Lock() // the poll departs without the result, as Poll's defer does
	j.pollers--
	orphan := j.pollers == 0 && !j.consumed
	j.mu.Unlock()
	require.True(t, orphan)
	m.onComplete(j)

	require.Equal(t, 1, c.noticeCount())
	assert.Equal(t, "Sub-agent "+id+" completed", c.lastNotice())
	ins := m.Boundary()
	require.Len(t, ins, 1)
	assert.Contains(t, ins[0].Text, id)
}

// TestOnCompleteIgnoresRunningJob covers the guard the orphan recovery relies on:
// a poll leaving a job that is still running must not queue a completion.
func TestOnCompleteIgnoresRunningJob(t *testing.T) {
	t.Parallel()
	c := newCapture()
	m := New(Options{
		Provider: func(llm.Model) (llm.Provider, error) { return &blockingProvider{}, nil },
		Notice:   func(msg string) { c.mu.Lock(); c.notices = append(c.notices, msg); c.mu.Unlock() },
	})
	t.Cleanup(m.Close)

	id := m.Start("x", "")
	j, ok := m.lookup(id)
	require.True(t, ok)

	m.onComplete(j) // the job has not finished; nothing to deliver
	assert.Zero(t, c.noticeCount())
	assert.Empty(t, m.Boundary())
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
	assert.LessOrEqual(t, g.peak.Load(), int32(max))
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

	// a job's row belongs to the job, not to one turn: a child that produced no
	// summary is nudged into another turn, and the row must not blink out between them.
	t.Run("row_survives_nudge_turn", func(t *testing.T) {
		toolTurn := []llm.Event{
			{Type: llm.EventToolCallStart, Index: 0, ToolCallID: "c1", ToolName: "read"},
			{Type: llm.EventToolCallEnd, Index: 0, Block: llm.ToolCallBlock{
				ID: "c1", Name: "read", Input: json.RawMessage(`{"path":"x.go"}`)}},
			{Type: llm.EventDone, StopReason: llm.StopToolUse},
		}
		p, _ := scripted([]llm.ScriptedTurn{
			{Events: thinkingOnlyTurn()}, // no text; the run nudges for a summary
			{Events: toolTurn},
			{Events: summaryTurn("final", llm.Usage{})},
		})
		c := newCapture()
		m := New(Options{
			Provider: p,
			Tools: &fakeSource{tools: []agent.Tool{&fakeTool{name: "read", result: "ok"}},
				readOnly: map[string]bool{"read": true}},
			Activity: c.recordRow,
		})
		t.Cleanup(m.Close)

		id := m.Start("task", "")
		j, ok := m.Poll(t.Context(), id)
		require.True(t, ok)
		require.Equal(t, StatusDone, j.Status)

		c.mu.Lock()
		rows := slices.Clone(c.rows)
		ranks := slices.Clone(c.ranks)
		c.mu.Unlock()

		// exactly one clear, and it is the last publish: spawn's terminal clear
		var clears []int
		for i, r := range rows {
			if r == id+"|" {
				clears = append(clears, i)
			}
		}
		require.Len(t, clears, 1, "the row is cleared once, at completion: %q", rows)
		assert.Equal(t, len(rows)-1, clears[0])
		for _, rank := range ranks { // every publish carries the job number as its rank
			assert.Equal(t, 1, rank)
		}
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
	assert.Equal(t, before, parent.Context().Used)
}

// jobStatus returns id's live status, with ok=false when unknown.
func jobStatus(m *Manager, id string) (Status, bool) {
	j, ok := m.lookup(id)
	if !ok {
		return 0, false
	}
	return j.statusOf(), true
}
