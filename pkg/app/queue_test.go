package app

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tui"
)

// stagePair returns the method values production wiring supplies: a real stager
// over no registry, so nothing can ever stage.
func stagePair(t *testing.T) (func(context.Context), func() []agent.MessageInfo) {
	t.Helper()

	stager := command.NewStager(t.Context(), nil, agent.NopSink{})
	return stager.WaitReady, stager.TakeReady
}

// fakeQueueUI records every call the steer queue makes, so tests assert on what reached the TUI.
type fakeQueueUI struct {
	queued    [][]string
	echoed    []string
	prepended []string
	set       []string
	images    [][]tui.Image
}

func (f *fakeQueueUI) SetQueued(texts []string) { f.queued = append(f.queued, slices.Clone(texts)) }
func (f *fakeQueueUI) PrependInput(t string)    { f.prepended = append(f.prepended, t) }
func (f *fakeQueueUI) SetInput(t string)        { f.set = append(f.set, t) }
func (f *fakeQueueUI) UserEcho(t string)        { f.echoed = append(f.echoed, t) }
func (f *fakeQueueUI) UserImages(imgs []tui.Image) {
	f.images = append(f.images, imgs)
}

// lastQueued returns the most recent queued-rows snapshot.
func (f *fakeQueueUI) lastQueued() []string {
	if len(f.queued) == 0 {
		return nil
	}
	return f.queued[len(f.queued)-1]
}

func TestSteerQueue(t *testing.T) {
	t.Parallel()

	t.Run("drain_live", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		assert.False(t, q.drainLive())
		q.offer(agent.Input{Text: "a"}, "a", 0) // marks draining, like the pump
		assert.True(t, q.drainLive())
		q.offer(agent.Input{Text: "b"}, "b", 0) // queues behind the drain
		assert.True(t, q.drainLive())

		_, ok := q.take(t.Context())
		assert.True(t, ok) // b hands over, the drain keeps running
		assert.True(t, q.drainLive())

		_, ok = q.take(t.Context())
		assert.False(t, ok) // empty: the drain goroutine is done
		assert.False(t, q.drainLive())
	})

	t.Run("idle_restored_when_current", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		var writes []bool
		q.settleIdle(q.nextGen(), func(idle bool) { writes = append(writes, idle) })

		assert.Equal(t, []bool{true}, writes)
	})

	t.Run("idle_skipped_after_newer", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		stale := q.nextGen()
		q.nextGen() // a newer dispatch stamps before its own SetIdle(false)

		var writes []bool
		q.settleIdle(stale, func(idle bool) { writes = append(writes, idle) })

		assert.Empty(t, writes)
	})

	t.Run("restore_holds_queue_lock", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		gen := q.nextGen()
		entered := make(chan struct{})
		release := make(chan struct{})
		go func() {
			q.settleIdle(gen, func(bool) {
				close(entered)
				<-release
			})
		}()
		<-entered // the restore is mid-write

		require.False(t, q.mu.TryLock()) // the write shares q.mu with nextGen's stamp
		close(release)
	})

	t.Run("offer_queues_while_draining", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var subs []int
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(e int) { subs = append(subs, e) }, func() {}, waitReady, takeReady)

		assert.False(t, q.offer(agent.Input{Text: "a"}, "alpha", 5))
		require.Empty(t, fake.queued)

		// a second submit while draining queues and re-renders rows + accounting
		assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 7))
		assert.Equal(t, []string{"beta"}, fake.lastQueued())
		assert.Equal(t, []int{7}, subs)
	})

	t.Run("pending_counts", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		assert.Equal(t, 0, q.pending())
		q.offer(agent.Input{Text: "seed"}, "one", 1) // starts the drain, not queued
		assert.Equal(t, 0, q.pending())

		require.True(t, q.offer(agent.Input{Text: "a"}, "alpha", 2))
		assert.Equal(t, 1, q.pending())
		require.True(t, q.offer(agent.Input{Text: "b"}, "beta", 3))
		assert.Equal(t, 2, q.pending())

		q.take(t.Context())
		assert.Equal(t, 0, q.pending())
	})

	t.Run("pull_joins_and_delivers", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var cleared int
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() { cleared++ }, waitReady, takeReady)

		inA := agent.Input{Text: "first", Before: []agent.MessageInfo{{Message: llm.Message{Role: llm.RoleUser}}}}
		inB := agent.Input{Text: "second", Before: []agent.MessageInfo{{Message: llm.Message{Role: llm.RoleAssistant}}}}
		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(inA, "label one", 3))
		require.True(t, q.offer(inB, "label two", 4))

		out := q.pull()
		require.Len(t, out, 1)
		assert.Equal(t, "first\nsecond", out[0].Text)
		assert.Len(t, out[0].Before, 2)
		assert.Nil(t, out[0].After)

		out[0].Delivered() // fires once the batch lands
		assert.Contains(t, fake.echoed, "label one\nlabel two")
		require.GreaterOrEqual(t, cleared, 1)
	})

	t.Run("hold_queues_without_drain", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var subs []int
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(e int) { subs = append(subs, e) }, func() {}, waitReady, takeReady)

		// the pump offers before it knows the input is held, so hold must undo that
		// drain mark: a queued row with no drain running, tokens still reserved
		require.False(t, q.offer(agent.Input{}, "", 0))
		q.hold(agent.Input{Text: "held"}, "held", 5)
		assert.Equal(t, []string{"held"}, fake.lastQueued())
		assert.Equal(t, []int{5}, subs)
		assert.False(t, q.draining) // the gated submit starts its own drain
		require.Equal(t, 1, q.pending())

		out, ok := q.take(t.Context()) // delivers once the gate passes and a turn runs
		require.True(t, ok)
		require.Len(t, out, 1)
		assert.Equal(t, "held", out[0].Text)

		_, ok = q.take(t.Context())
		assert.False(t, ok) // drained; the next offer starts a fresh drain
	})

	t.Run("delivered_echoes_images", func(t *testing.T) {
		var pngBuf bytes.Buffer
		require.NoError(t, png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 4, 4))))

		fake := &fakeQueueUI{}
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady)

		in := agent.Input{
			Text:   "look",
			Blocks: llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: pngBuf.Bytes()}},
		}
		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(in, "look", 2))

		out := q.pull()
		require.Len(t, out, 1)
		out[0].Delivered()
		assert.Contains(t, fake.echoed, "look")
		require.Len(t, fake.images, 1)
		require.Len(t, fake.images[0], 1)
		assert.Equal(t, "[image 4x4, "+strutil.HumanSize(int64(pngBuf.Len()))+"]", fake.images[0][0].String())
	})

	t.Run("image_only_row_placeholder", func(t *testing.T) {
		fake := &fakeQueueUI{}
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady)

		// an image-only submission has no typed line: the placeholder names it in rows
		png, wide := slotPNG(t), slotSizedPNG(t, 8, 2)
		in := agent.Input{Blocks: llm.BlockList{
			llm.ImageBlock{MediaType: "image/png", Data: png},
			llm.ImageBlock{MediaType: "image/png", Data: wide},
		}}
		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(in, "", 2))

		want := llm.ImagePlaceholder(png, "") + ", " + llm.ImagePlaceholder(wide, "")
		assert.Equal(t, []string{want}, fake.lastQueued())
	})

	t.Run("delivered_image_only_visible", func(t *testing.T) {
		fake := &fakeQueueUI{}
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady)

		in := agent.Input{
			Blocks: llm.BlockList{llm.ImageBlock{MediaType: "image/png", Data: slotPNG(t)}},
		}
		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(in, "", 2))

		out := q.pull()
		require.Len(t, out, 1)
		assert.Empty(t, fake.echoed)

		out[0].Delivered() // fires at landing: UserImages alone names an image-only turn
		assert.Empty(t, fake.echoed)
		require.Len(t, fake.images, 1)
		require.Len(t, fake.images[0], 1)
	})

	t.Run("join_splits_provenance", func(t *testing.T) {
		fake := &fakeQueueUI{}
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady)

		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "user text"}, "typed", 1))
		require.True(t, q.offer(agent.Input{Text: "system notice", Injected: true}, "notice", 1))
		require.True(t, q.offer(agent.Input{Text: "more user text"}, "typed two", 1))

		runs := q.pull()
		require.Len(t, runs, 3)
		assert.False(t, runs[0].Injected)
		assert.Equal(t, "user text", runs[0].Text)
		assert.True(t, runs[1].Injected)
		assert.Equal(t, "system notice", runs[1].Text)
		assert.False(t, runs[2].Injected)
		assert.Equal(t, "more user text", runs[2].Text)

		runs[0].Delivered()
		runs[2].Delivered()
		assert.Equal(t, []string{"typed", "typed two"}, fake.echoed)
	})

	t.Run("join_chains_afters_per_run", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
		after := func(text string) func(context.Context) []llm.Message {
			return func(context.Context) []llm.Message {
				return []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: text}}}}
			}
		}

		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "a", After: after("read a")}, "a", 1))
		require.True(t, q.offer(agent.Input{Text: "sys", Injected: true, After: after("read sys")}, "sys", 1))

		runs := q.pull()
		require.Len(t, runs, 2)
		msgs := runs[0].After(t.Context())
		require.Len(t, msgs, 1)
		assert.Equal(t, llm.TextBlock{Text: "read a"}, msgs[0].Content[0])
		msgs = runs[1].After(t.Context())
		require.Len(t, msgs, 1)
		assert.Equal(t, llm.TextBlock{Text: "read sys"}, msgs[0].Content[0])
	})

	t.Run("join_preserves_prepared", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		// every queued item is pump-expanded (Prepared), and the runs must stay so the
		// append seam never re-expands an annotate-only reference at delivery
		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "see @big.bin", Prepared: true}, "typed", 1))
		require.True(t, q.offer(agent.Input{Text: "sys", Injected: true, Prepared: true}, "sys", 1))

		runs := q.pull()
		require.Len(t, runs, 2)
		assert.True(t, runs[0].Prepared)
		assert.True(t, runs[1].Prepared)
	})

	t.Run("join_mixed_prepared", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		// a run is prepared only when every item in it is, so raw text still expands
		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "expanded", Prepared: true}, "a", 1))
		require.True(t, q.offer(agent.Input{Text: "raw @a.go"}, "b", 1))

		runs := q.pull()
		require.Len(t, runs, 1)
		assert.False(t, runs[0].Prepared)
		assert.Equal(t, "expanded\nraw @a.go", runs[0].Text)
	})

	t.Run("take_joins_batch", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)

		q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain, not queued
		require.True(t, q.draining)
		assert.True(t, q.offer(agent.Input{Text: "a"}, "alpha", 1))
		assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 2))

		ins, ok := q.take(t.Context())
		require.True(t, ok)
		require.Len(t, ins, 1)
		assert.Equal(t, "a\nb", ins[0].Text)

		_, ok = q.take(t.Context()) // empty now: clears draining so a later offer starts fresh
		assert.False(t, ok)
		assert.False(t, q.draining)
	})

	t.Run("stop_drain_keeps_items", func(t *testing.T) {
		fake := &fakeQueueUI{}
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady, takeReady)

		q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain
		require.True(t, q.draining)
		assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 2))
		require.Len(t, fake.lastQueued(), 1)

		q.stopDrain() // a failing provider must not be hammered

		assert.False(t, q.draining)
		assert.Len(t, fake.lastQueued(), 1)
	})

	t.Run("recall_pops_lifo", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var refreshes int // submit-bucket updates on each pop
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) { refreshes++ }, func() {}, waitReady, takeReady)

		q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain, not queued
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
	})

	t.Run("abort_recovers_all", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var cleared int
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() { cleared++ }, waitReady, takeReady)

		q.offer(agent.Input{Text: "first"}, "one", 0) // starts the drain, not queued
		assert.True(t, q.offer(agent.Input{Text: "b"}, "beta", 2))
		assert.True(t, q.offer(agent.Input{Text: "c"}, "gamma", 3))

		q.abort() // interrupt path: everything returns to the editor before cancel

		assert.Equal(t, []string{"beta\ngamma"}, fake.prepended)
		assert.Empty(t, fake.lastQueued())
		require.GreaterOrEqual(t, cleared, 1)
	})

	t.Run("abort_clears_empty_reserve", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var cleared int
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() { cleared++ }, waitReady, takeReady)

		// a direct dispatch: offer only marked the drain, nothing queued, and the
		// pump reserved the estimate itself before the turn parked in the hold
		require.False(t, q.offer(agent.Input{Text: "direct"}, "direct", 0))

		q.abort() // interrupt path with nothing queued

		assert.Empty(t, fake.prepended) // no rows: the editor draft stays untouched
		assert.Empty(t, fake.lastQueued())
		require.GreaterOrEqual(t, cleared, 1) // the submit reserve releases anyway
	})

	t.Run("take_attaches_staged", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var consumed int
		waitReady, _ := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady,
			func() []agent.MessageInfo {
				consumed++
				return []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, "User Ran: make")}}
			})

		q.offer(agent.Input{Text: "seed"}, "seed", 0) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "why did it fail"}, "why did it fail", 1))

		in, ok := q.take(t.Context())
		require.True(t, ok)
		require.Len(t, in, 1)
		// the staged output rides ahead of the queued item's own Before, verbatim
		require.Len(t, in[0].Before, 1)
		assert.Equal(t, llm.Text(llm.RoleUser, "User Ran: make"), in[0].Before[0].Message)
		assert.Equal(t, 1, consumed)
	})

	t.Run("pull_attaches_staged", func(t *testing.T) {
		waitReady, _ := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady,
			func() []agent.MessageInfo {
				return []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, "User Ran: make")}}
			})

		q.offer(agent.Input{Text: "seed"}, "seed", 0) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "and then?"}, "and then?", 1))

		in := q.pull()
		require.Len(t, in, 1)
		require.Len(t, in[0].Before, 1)
		assert.Equal(t, llm.Text(llm.RoleUser, "User Ran: make"), in[0].Before[0].Message)
	})

	t.Run("take_aborted_while_waiting", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var consumed bool
		waitReady, _ := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady,
			func() []agent.MessageInfo {
				consumed = true
				return nil
			})

		q.offer(agent.Input{Text: "seed"}, "seed", 0) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "late"}, "late", 1))

		// waitReady parks, an Esc aborts the queue while take holds nothing
		entered := make(chan struct{})
		release := make(chan struct{})
		q.waitReady = func(context.Context) {
			close(entered)
			<-release
		}
		type takeResult struct {
			in []agent.Input
			ok bool
		}
		res := make(chan takeResult, 1)
		go func() {
			in, ok := q.take(t.Context())
			res <- takeResult{in: in, ok: ok}
		}()
		<-entered
		q.abort()
		close(release)

		got := <-res
		require.False(t, got.ok) // nothing delivers: the abort emptied the queue
		assert.Empty(t, got.in)
		assert.False(t, consumed) // the staged output was never consumed, still in the stager
	})

	t.Run("abort_never_consumes_staged", func(t *testing.T) {
		fake := &fakeQueueUI{}
		var consumed bool
		waitReady, _ := stagePair(t)
		q := newSteerQueue(fake, func(int) {}, func() {}, waitReady,
			func() []agent.MessageInfo {
				consumed = true
				return nil
			})

		// a queued /init survey carries its own Before; abort recovers the label only
		in := agent.Input{Text: "", Before: []agent.MessageInfo{{Message: llm.Text(llm.RoleUser, "survey pair")}}}
		q.offer(agent.Input{Text: "seed"}, "seed", 0) // starts the drain, not queued
		require.True(t, q.offer(in, "/init", 1))

		q.abort()
		assert.False(t, consumed) // staged shell output is the stager's until delivery
		assert.Equal(t, []string{"/init"}, fake.prepended)
	})
}

func TestJoinAfter(t *testing.T) {
	t.Parallel()

	t.Run("no_resolvers", func(t *testing.T) {
		assert.Nil(t, joinAfter(nil))
	})

	t.Run("chains_in_submit_order", func(t *testing.T) {
		waitReady, takeReady := stagePair(t)
		q := newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}, waitReady, takeReady)
		after := func(text string) func(context.Context) []llm.Message {
			return func(context.Context) []llm.Message {
				return []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: text}}}}
			}
		}

		q.offer(agent.Input{Text: "seed"}, "seed", 1) // starts the drain, not queued
		require.True(t, q.offer(agent.Input{Text: "a", After: after("read a")}, "a", 1))
		require.True(t, q.offer(agent.Input{Text: "b"}, "b", 1)) // no reads, must not break the chain
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
