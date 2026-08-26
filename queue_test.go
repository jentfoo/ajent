package main

import (
	"context"
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeQueueUI records every call the steer queue makes, so tests assert on what
// reached the TUI without one.
type fakeQueueUI struct {
	queued    [][]string
	echoed    []string
	prepended []string
	set       []string
}

func (f *fakeQueueUI) SetQueued(texts []string) { f.queued = append(f.queued, slices.Clone(texts)) }
func (f *fakeQueueUI) PrependInput(t string)    { f.prepended = append(f.prepended, t) }
func (f *fakeQueueUI) SetInput(t string)        { f.set = append(f.set, t) }
func (f *fakeQueueUI) UserEcho(t string)        { f.echoed = append(f.echoed, t) }

// lastQueued returns the most recent queued-rows snapshot.
func (f *fakeQueueUI) lastQueued() []string {
	if len(f.queued) == 0 {
		return nil
	}
	return f.queued[len(f.queued)-1]
}

func TestSteerQueueOffer(t *testing.T) {
	t.Parallel()

	fake := &fakeQueueUI{}
	var subs []int
	q := newSteerQueue(fake, func(e int) { subs = append(subs, e) }, func() {})

	assert.False(t, q.offer(agent.Input{Text: "a"}, "alpha", 5))
	require.Empty(t, fake.queued)

	// a second submit while draining queues and re-renders rows + accounting
	assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 7))
	assert.Equal(t, []string{"beta"}, fake.lastQueued())
	assert.Equal(t, []int{7}, subs)
}

func TestSteerQueuePullJoinsAndDelivers(t *testing.T) {
	t.Parallel()

	fake := &fakeQueueUI{}
	var cleared int
	q := newSteerQueue(fake, func(int) {}, func() { cleared++ })

	inA := agent.Input{Text: "first", Before: []llm.Message{{Role: llm.RoleUser}}}
	inB := agent.Input{Text: "second", Before: []llm.Message{{Role: llm.RoleAssistant}}}
	q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain; not queued
	require.True(t, q.offer(inA, "label one", 3))
	require.True(t, q.offer(inB, "label two", 4))

	out := q.pull()
	require.Len(t, out, 1)
	assert.Equal(t, "first\nsecond", out[0].Text)
	assert.Len(t, out[0].Before, 2)
	assert.Nil(t, out[0].After, "no queued item carried @ reads")

	out[0].Delivered() // fires once the batch lands
	assert.Contains(t, fake.echoed, "label one\nlabel two")
	require.GreaterOrEqual(t, cleared, 1)
}

func TestJoinAfter(t *testing.T) {
	t.Parallel()

	t.Run("no_resolvers", func(t *testing.T) {
		assert.Nil(t, joinAfter(nil))
	})

	t.Run("chains_in_submit_order", func(t *testing.T) {
		fake := &fakeQueueUI{}
		q := newSteerQueue(fake, func(int) {}, func() {})
		after := func(text string) func(context.Context) []llm.Message {
			return func(context.Context) []llm.Message {
				return []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: text}}}}
			}
		}

		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain; not queued
		require.True(t, q.offer(agent.Input{Text: "a", After: after("read a")}, "a", 1))
		require.True(t, q.offer(agent.Input{Text: "b"}, "b", 1)) // no reads; must not break the chain
		require.True(t, q.offer(agent.Input{Text: "c", After: after("read c")}, "c", 1))

		out := q.pull()
		require.Len(t, out, 1)
		require.NotNil(t, out[0].After)

		msgs := out[0].After(t.Context())
		require.Len(t, msgs, 2)
		assert.Equal(t, llm.TextBlock{Text: "read a"}, msgs[0].Content[0])
		assert.Equal(t, llm.TextBlock{Text: "read c"}, msgs[1].Content[0])
	})
}

func TestSteerQueueTake(t *testing.T) {
	t.Parallel()

	fake := &fakeQueueUI{}
	q := newSteerQueue(fake, nil, nil)

	q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain; not queued
	require.True(t, q.draining)
	assert.True(t, q.offer(agent.Input{Text: "a"}, "alpha", 1))
	assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 2))

	in, ok := q.take()
	require.True(t, ok)
	assert.Equal(t, "a\nb", in.Text)

	_, ok = q.take() // empty now: clears draining so a later offer starts fresh
	assert.False(t, ok)
	assert.False(t, q.draining)
}

func TestSteerQueueStopDrainKeepsItems(t *testing.T) {
	t.Parallel()

	fake := &fakeQueueUI{}
	q := newSteerQueue(fake, nil, nil)

	q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain
	require.True(t, q.draining)
	assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 2))
	require.Len(t, fake.lastQueued(), 1)

	q.stopDrain() // a failing provider must not be hammered

	assert.False(t, q.draining)
	assert.Len(t, fake.lastQueued(), 1)
}

func TestSteerQueueRecall(t *testing.T) {
	t.Parallel()

	fake := &fakeQueueUI{}
	var refreshes int // submit-bucket updates on each pop
	q := newSteerQueue(fake, func(int) { refreshes++ }, func() {})

	q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain; not queued
	assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 5))
	assert.True(t, q.offer(agent.Input{Text: "c"}, "gamma", 7))

	require.True(t, q.recall()) // LIFO: newest message back into the editor
	assert.Equal(t, []string{"gamma"}, fake.prepended)
	assert.Len(t, fake.lastQueued(), 1)

	require.True(t, q.recall())
	assert.Equal(t, []string{"gamma", "beta"}, fake.prepended)
	assert.Empty(t, fake.lastQueued())

	assert.False(t, q.recall())
	require.GreaterOrEqual(t, refreshes, 2)
}

func TestSteerQueueAbortRecoversAll(t *testing.T) {
	t.Parallel()

	fake := &fakeQueueUI{}
	var cleared int
	q := newSteerQueue(fake, func(int) {}, func() { cleared++ })

	q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain; not queued
	assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 2))
	assert.True(t, q.offer(agent.Input{Text: "c"}, "gamma", 3))

	q.abort() // interrupt path: everything returns to the editor before cancel

	assert.Equal(t, []string{"beta\ngamma"}, fake.prepended)
	assert.Empty(t, fake.lastQueued())
	require.GreaterOrEqual(t, cleared, 1)
}
