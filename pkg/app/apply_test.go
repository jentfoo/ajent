package app

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// recSurface records every eventSurface call an applier makes, so chrome
// mapping and snapshot reconcile are assertable without a real terminal. The
// maps hold last-writer state exactly like the surface behind them.
type recSurface struct {
	notices []string // level|key|text rows in arrival order
	prints  []string
	segs    map[string]tui.Segment
	models  []string
	acts    map[string]string // key to last text
	queue   [][]string        // every SetQueued replacement, oldest first
	idles   []bool            // every SetIdle call in order
}

func newRecSurface() *recSurface {
	return &recSurface{
		segs: make(map[string]tui.Segment),
		acts: make(map[string]string),
	}
}

func (r *recSurface) Notify(msg string, level tui.Level) {
	r.notices = append(r.notices, levelName(agent.Level(level))+"||"+msg)
}

func (r *recSurface) NotifyKeyed(key, msg string, level tui.Level) {
	r.notices = append(r.notices, levelName(agent.Level(level))+"|"+key+"|"+msg)
}

func (r *recSurface) Print(markdown string) { r.prints = append(r.prints, markdown) }

func (r *recSurface) SetStatusSegment(seg tui.Segment) {
	if seg.Text == "" {
		delete(r.segs, seg.Key)
		return
	}
	r.segs[seg.Key] = seg
}

func (r *recSurface) SetModel(name, short string, maxTokens int) {
	r.models = append(r.models, name+"|"+short)
}

func (r *recSurface) SetActivityRanked(key, text string, rank int) {
	if text == "" {
		delete(r.acts, key)
		return
	}
	r.acts[key] = text
}

func (r *recSurface) SetQueued(rows []string) { r.queue = append(r.queue, rows) }

func (r *recSurface) SetIdle(idle bool) { r.idles = append(r.idles, idle) }

// testApplier builds an applier over rec with a real sink behind it, since
// stream families always render through tui/sink.
func testApplier(t *testing.T, rec *recSurface, hooks applyHooks) *applier {
	t.Helper()
	term, _ := pipeUI(t)
	return newApplier(rec, tuisink.New(term), hooks)
}

// TestApplierChromeEvents pins the shared chrome mapping: each event family
// reaches exactly one surface call, keyed notices replace rather than stack,
// empty texts remove, and placement comes from this build's spec table.
func TestApplierChromeEvents(t *testing.T) {
	t.Parallel()

	t.Run("unkeyed_notice", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventNotice, 1, ctl.NoticeData{Text: "plain", Level: "warn"}))
		assert.Equal(t, []string{"warn||plain"}, rec.notices)
	})

	t.Run("keyed_notice", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventNotice, 1, ctl.NoticeData{Key: "subagent", Text: "one job"}))
		assert.Equal(t, []string{"info|subagent|one job"}, rec.notices)
	})

	t.Run("print_markdown", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventPrint, 1, ctl.PrintData{Markdown: "# heading"}))
		assert.Equal(t, []string{"# heading"}, rec.prints)
	})

	t.Run("segment_spec_placement", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventStatus, 1,
			ctl.StatusData{Segment: &ctl.Segment{Key: segMCP, Text: "mcp 1/2", Short: "1/2"}}))
		want := segment(segMCP, "mcp 1/2", "1/2") // order and priority from the spec table
		assert.Equal(t, want, rec.segs[segMCP])
	})

	t.Run("segment_removal", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventStatus, 1,
			ctl.StatusData{Segment: &ctl.Segment{Key: segMCP, Text: "mcp 1/2"}}))
		ap.Apply(ctl.NewEvent(ctl.EventStatus, 2,
			ctl.StatusData{Segment: &ctl.Segment{Key: segMCP}})) // empty text removes
		assert.Empty(t, rec.segs)
	})

	t.Run("model_labels", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventStatus, 1,
			ctl.StatusData{Model: &ctl.ModelState{Key: "p/big", Short: "big", Window: 2000}}))
		assert.Equal(t, []string{"p/big|big"}, rec.models)
	})

	t.Run("activity_rows", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventActivity, 1, ctl.ActivityData{Key: "job:1", Text: "reading", Rank: 3}))
		ap.Apply(ctl.NewEvent(ctl.EventActivity, 2, ctl.ActivityData{Key: "job:1"})) // removal
		assert.Empty(t, rec.acts)
	})

	t.Run("queued_default_texts", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventQueued, 1,
			ctl.QueuedData{Rows: []ctl.QueueRow{{ID: "q1", Text: "waiting"}}}))
		require.Len(t, rec.queue, 1)
		assert.Equal(t, []string{"waiting"}, rec.queue[0])
	})

	t.Run("idle_flip", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventIdle, 1, ctl.IdleData{Idle: false}))
		assert.Equal(t, []bool{false}, rec.idles)
	})

	t.Run("session_facts_stay_silent", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		for _, ev := range []ctl.Event{
			ctl.NewEvent(ctl.EventSessionRenamed, 1, ctl.SessionRenamedData{Name: "x"}),
			ctl.NewEvent(ctl.EventPermission, 2, ctl.PermissionData{Mode: "auto"}),
			ctl.NewEvent(ctl.EventTools, 3, ctl.ToolsData{}),
			ctl.NewEvent(ctl.EventEditor, 4, ctl.EditorData{Text: "draft"}),
			ctl.NewEvent(ctl.EventUsage, 5, ctl.UsageData{}),
		} {
			ap.Apply(ev)
		}
		assert.Empty(t, rec.notices)
		assert.Empty(t, rec.prints)
		assert.Empty(t, rec.segs)
		assert.Empty(t, rec.models)
		assert.Empty(t, rec.acts)
	})
}

// TestApplierQueueHook pins the queue merge point: a surface merges its own
// local state into the wire copy through the hook, and a hookless applier
// falls back to the plain texts.
func TestApplierQueueHook(t *testing.T) {
	t.Parallel()

	rec := newRecSurface()
	var merged []ctl.QueueRow
	ap := testApplier(t, rec, applyHooks{
		queueRows: func(rows []ctl.QueueRow) []string {
			merged = rows
			return []string{"display row"}
		},
	})
	ap.Apply(ctl.NewEvent(ctl.EventQueued, 1,
		ctl.QueuedData{Rows: []ctl.QueueRow{{ID: "q1", Text: "waiting"}}}))

	require.Len(t, rec.queue, 1)
	assert.Equal(t, []string{"display row"}, rec.queue[0])
	require.Len(t, merged, 1) // the hook saw the wire copy
	assert.Equal(t, "q1", merged[0].ID)
}

// TestApplierSnapshotReconciles pins the diff-clear on snapshot: rows and
// segments the new state no longer lists clear instead of lingering, an emptied
// queue clears behind its old guard, a turn that ended while the surface was
// away forces its end, and the snapshot has the last word on idle.
func TestApplierSnapshotReconciles(t *testing.T) {
	t.Parallel()

	newLive := func(t *testing.T) (*applier, *recSurface, *[]bool) {
		t.Helper()
		rec := newRecSurface()
		runs := &[]bool{}
		hooks := applyHooks{
			running: func(live bool) {
				*runs = append(*runs, live)
				rec.SetIdle(!live) // the attach view paints idle from this hook too
			},
		}
		ap := testApplier(t, rec, hooks)
		// chrome painted live before the drop, one row of each family to keep
		ap.Apply(ctl.NewEvent(ctl.EventStatus, 1,
			ctl.StatusData{Segment: &ctl.Segment{Key: segMCP, Text: "mcp 1/2"}}))
		ap.Apply(ctl.NewEvent(ctl.EventStatus, 2,
			ctl.StatusData{Segment: &ctl.Segment{Key: segHint, Text: "hold on"}}))
		ap.Apply(ctl.NewEvent(ctl.EventActivity, 3, ctl.ActivityData{Key: "job:1", Text: "reading", Rank: 1}))
		ap.Apply(ctl.NewEvent(ctl.EventActivity, 4, ctl.ActivityData{Key: "job:2", Text: "scanning", Rank: 2}))
		ap.Apply(ctl.NewEvent(ctl.EventTurnStart, 5, ctl.TurnStartData{}))
		return ap, rec, runs
	}

	t.Run("clears_removed_rows", func(t *testing.T) {
		ap, rec, _ := newLive(t)

		keepSeg := segment(segHint, "hold on", "")
		keepAct := "scanning"
		ap.Snapshot(ctl.State{
			Status:   ctl.StatusRow{Segments: []ctl.Segment{{Key: keepSeg.Key, Text: keepSeg.Text, Short: keepSeg.Short}}},
			Activity: []ctl.ActivityRow{{Key: keepAct, Text: keepAct, Rank: 2}},
			Idle:     true,
		})

		assert.Equal(t, map[string]tui.Segment{keepSeg.Key: keepSeg}, rec.segs)
		assert.Equal(t, map[string]string{keepAct: keepAct}, rec.acts)
		assert.False(t, ap.live()) // the turn ended while this surface was away
	})

	t.Run("forces_lost_turn_end", func(t *testing.T) {
		ap, _, runs := newLive(t)

		ap.Snapshot(ctl.State{Idle: true}) // spinning false with a live turn: reconcile

		assert.Equal(t, []bool{true, false}, *runs)
	})

	t.Run("spinning_snapshot_keeps_turn", func(t *testing.T) {
		ap, _, runs := newLive(t)

		ap.Snapshot(ctl.State{Spinning: true}) // the same turn still runs on the host

		assert.Equal(t, []bool{true}, *runs) // no second start, no forced end
		assert.True(t, ap.live())
	})

	t.Run("replaces_queue_wholesale", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})
		ap.Apply(ctl.NewEvent(ctl.EventQueued, 1,
			ctl.QueuedData{Rows: []ctl.QueueRow{{ID: "q1", Text: "waiting"}}}))
		require.NotEmpty(t, rec.queue[len(rec.queue)-1])

		// the emptied queue clears even though nothing pending remains to merge
		ap.Snapshot(ctl.State{})
		assert.Empty(t, rec.queue[len(rec.queue)-1])
	})

	t.Run("idle_has_last_word", func(t *testing.T) {
		ap, rec, _ := newLive(t)

		ap.Snapshot(ctl.State{Idle: true}) // the forced end paints rest first

		require.NotEmpty(t, rec.idles)
		assert.True(t, rec.idles[len(rec.idles)-1])
	})

	t.Run("empty_model_keeps_labels", func(t *testing.T) {
		rec := newRecSurface()
		ap := testApplier(t, rec, applyHooks{})

		ap.Snapshot(ctl.State{}) // a snapshot without a model says nothing about it

		assert.Empty(t, rec.models)
	})
}

// TestApplierLateToolHeader pins the lost-start rule: a tool result whose start
// event this surface never saw renders its header here, attached to its body.
func TestApplierLateToolHeader(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	ap := newApplier(term, tuisink.New(term), applyHooks{})

	ap.Apply(ctl.NewEvent(ctl.EventToolResult, 1,
		ctl.ToolResultData{ID: "t9", Name: "grep", Output: "the hit"}))

	require.Eventually(t, func() bool {
		o := out()
		return o != "" && containsAll(o, "grep", "the hit")
	}, 2*time.Second, 5*time.Millisecond)
	assert.NotContains(t, out(), "tool grep failed")
}

// TestApplierStreamedResultNotRepeated pins the other half of the fallback: a
// call whose deltas already rendered commits nothing from its result copy.
func TestApplierStreamedResultNotRepeated(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	ap := newApplier(term, tuisink.New(term), applyHooks{})

	ap.Apply(ctl.NewEvent(ctl.EventToolStart, 1, ctl.ToolStartData{ID: "t1", Name: "bash"}))
	ap.Apply(ctl.NewEvent(ctl.EventToolOutput, 2, ctl.ToolOutputData{ID: "t1", Delta: "streamed line\n"}))
	ap.Apply(ctl.NewEvent(ctl.EventToolResult, 3,
		ctl.ToolResultData{ID: "t1", Name: "bash", Output: "streamed line\n"})) // the no-stream controller's copy

	require.Eventually(t, func() bool {
		return containsAll(out(), "streamed line")
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, 1, strings.Count(out(), "streamed line"))
}

// containsAll reports whether s carries every substring.
func containsAll(s string, wants ...string) bool {
	for _, w := range wants {
		if !strings.Contains(s, w) {
			return false
		}
	}
	return true
}
