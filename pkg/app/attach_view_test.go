package app

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tui"
)

// TestAttachBackfillRendersHistory pins the attach promise: committed history
// renders through replay and the snapshot paints the model label.
func TestAttachBackfillRendersHistory(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "what changed")
	})
	a := newAttachRig(t, rig)

	require.Eventually(t, func() bool { return strings.Contains(a.out(), "what changed") },
		ctlWait, 5*time.Millisecond)
}

// TestAttachRecallSources pins the recall scopes against a session that an
// earlier one came before: ctrl+r walks the workspace-wide recall store, so the
// prior session's prompt surfaces beside the attached session's own while
// injected context, assistant messages and secret-prefixed lines never do; and
// plain arrows stay scoped to the attached transcript alone. Both follow
// prompts the host records after attach through the files they read.
func TestAttachRecallSources(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "attached session prompt")
	})

	// an earlier session in the same workspace recorded one prompt and one secret
	prior, err := rig.rec.store.Create(config.Cwd(),
		session.SessionData{Version: session.Version(), Workspace: config.Cwd()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = prior.Close() })
	priorRec := &sessRec{rec: session.NewRecorder(prior)}
	priorRec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "earlier session prompt")})
	priorRec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "secret: hunter2")})

	// the attached transcript itself carries a second prompt, a reply and an
	// injected row that must never surface on either gesture
	writeMessage(t, rig.rec, "second prompt")
	rig.rec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleAssistant, "an answer")})
	rig.rec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "injected"), Injected: true})

	a := newAttachRig(t, rig)
	search, arrows := a.view.recallSources()
	require.NotNil(t, search)
	require.NotNil(t, arrows)

	texts := func(items []tui.SearchItem) []string {
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.Text
		}
		return out
	}
	st := texts(search()) // search spans sessions; secrets stay excluded at read time
	assert.Contains(t, st, "earlier session prompt")
	assert.Contains(t, st, "attached session prompt")
	assert.NotContains(t, st, "secret: hunter2")
	assert.NotContains(t, st, "injected")
	assert.NotContains(t, st, "an answer")

	at := arrows() // arrows read only the attached transcript, newest first
	assert.Equal(t, []string{"second prompt", "attached session prompt"}, at)

	// a later host-recorded prompt joins both through the files, no reattach
	writeMessage(t, rig.rec, "later prompt")
	require.Eventually(t, func() bool {
		_, arr := a.view.recallSources()
		return slices.Equal(arr(), []string{"later prompt", "second prompt", "attached session prompt"})
	}, ctlWait, 5*time.Millisecond)
}

// TestAttachRunSearchesHistory pins the ctrl+r wiring end to end: an attached
// view's search source loads from the wire transcript, and accepting the match
// fills this terminal's editor so submitting re-sends the recorded prompt.
func TestAttachRunSearchesHistory(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "searchable prompt")
	})

	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)
	a.view.installRecall()

	feed("\x12") // ctrl+r opens the search over the wire source
	require.Eventually(t, func() bool {
		ok, _ := term.Searching()
		return ok
	}, ctlWait, 5*time.Millisecond)

	feed("search") // typed keys filter; the match paints once the source delivers
	// the committed history line already painted one copy at backfill: only an
	// actually delivered, filtered overlay match adds a second
	require.Eventually(t, func() bool {
		return strings.Count(out(), "able prompt") >= 2
	}, ctlWait, 5*time.Millisecond)

	feed("\r\r") // accept the match into the editor, then submit it
	assert.Equal(t, "searchable prompt", <-term.Messages())
}

// TestAttachViewStreams pins surface parity for committed history: an echo,
// keyed and unkeyed notices, /command print output, text deltas and tool
// starts, outputs and results each reach the attached terminal as the host's
// renderer would show them. Status-line chrome (segments, queued rows,
// activity) forwards through the same tui setters the host uses; its rendering
// is pkg/tui's own tested behavior, not capturable in plain scrollback.
func TestAttachViewStreams(t *testing.T) {
	t.Run("live_events", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		// no echo exists: a host-side echo stays local, so only transient
		// notices and prints reach this surface
		rig.hub.ui.NotifyKeyed("subagent", "one job", tui.LevelInfo)
		rig.hub.ui.Print("## printed block")
		rig.hub.ui.Notify("plain notice", tui.LevelWarn)

		require.Eventually(t, func() bool {
			out := a.out()
			return strings.Contains(out, "one job") &&
				strings.Contains(out, "printed block") &&
				strings.Contains(out, "plain notice")
		}, ctlWait, 5*time.Millisecond)
	})

	t.Run("turn_text", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		rig.hub.publish(ctl.EventTextDelta, ctl.TextData{Delta: "partial "})
		rig.hub.publish(ctl.EventTextDelta, ctl.TextData{Delta: "answer"})
		rig.hub.publish(ctl.EventTextEnd, nil)

		require.Eventually(t, func() bool { return strings.Contains(a.out(), "partial answer") },
			ctlWait, 5*time.Millisecond)
	})

	t.Run("tool_header_and_result", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		rig.hub.publish(ctl.EventToolStart, ctl.ToolStartData{ID: "t1", Name: "bash", Label: "bash: ls"})
		rig.hub.publish(ctl.EventToolOutput, ctl.ToolOutputData{ID: "t1", Delta: "file.go\n"})
		rig.hub.publish(ctl.EventToolResult, ctl.ToolResultData{ID: "t1", Name: "bash"})

		require.Eventually(t, func() bool {
			out := a.out()
			return strings.Contains(out, "bash: ls") && strings.Contains(out, "file.go")
		}, ctlWait, 5*time.Millisecond)
	})

	t.Run("tool_streams_once", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		rig.hub.publish(ctl.EventToolStart, ctl.ToolStartData{ID: "t1", Name: "bash", Label: "bash: date"})
		rig.hub.publish(ctl.EventToolOutput,
			ctl.ToolOutputData{ID: "t1", Delta: "Wed Oct 28 16:32:04 MDT 2026\n"})
		rig.hub.publish(ctl.EventToolResult, ctl.ToolResultData{ID: "t1", Name: "bash",
			Output: "Wed Oct 28 16:32:04 MDT 2026\n"}) // the no-stream controller's copy

		// a notice published after the result commits later in the same order, so
		// seeing it proves every earlier commit (a duplicate included) has flushed
		rig.hub.publish(ctl.EventNotice, ctl.NoticeData{Text: "stream settled"})
		require.Eventually(t, func() bool {
			return strings.Contains(a.out(), "stream settled")
		}, ctlWait, 5*time.Millisecond)

		assert.Equal(t, 1, strings.Count(a.out(), "Wed Oct 28 16:32:04"))
	})

	t.Run("non_streaming_result", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		rig.hub.publish(ctl.EventToolStart, ctl.ToolStartData{ID: "t2", Name: "write"})
		rig.hub.publish(ctl.EventToolResult, ctl.ToolResultData{ID: "t2", Name: "write",
			Output: "wrote notes.go"})

		require.Eventually(t, func() bool {
			return strings.Count(a.out(), "wrote notes.go") == 1
		}, ctlWait, 5*time.Millisecond)
	})

	t.Run("idle_event_drives_rest_state", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		rig.hub.publish(ctl.EventIdle, ctl.IdleData{Idle: false})
		require.Eventually(t, func() bool { return !a.view.ui.Idle() },
			ctlWait, 5*time.Millisecond)
		rig.hub.publish(ctl.EventIdle, ctl.IdleData{Idle: true})
		require.Eventually(t, func() bool { return a.view.ui.Idle() },
			ctlWait, 5*time.Millisecond)
	})
}

// TestAttachToolResultEntryNoEcho pins the live-echo parity rule: a committed
// tool-result entry (user role, tool-result blocks only) is not a prompt, so
// the attached terminal never paints the empty user row the host does not
// show, whether a turn is streaming or at rest. A typed prompt still echoes.
func TestAttachToolResultEntryNoEcho(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	record := func(rec *sessRec) {
		rec.rec.Message(agent.MessageInfo{
			Message: llm.Message{Role: llm.RoleUser, Content: llm.BlockList{
				llm.ToolResultBlock{CallID: "t1", ToolName: "bash",
					Content: llm.BlockList{llm.TextBlock{Text: "denied by user"}}, IsError: true},
			}},
		})
	}

	t.Run("at_rest", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)
		record(rig.rec)
		writeMessage(t, rig.rec, "next prompt") // ordered after the entry under test

		require.Eventually(t, func() bool { return strings.Contains(a.out(), "next prompt") },
			ctlWait, 5*time.Millisecond)
		assert.NotContains(t, a.out(), "❯ \n")
	})

	t.Run("while_streaming", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)
		rig.hub.publish(ctl.EventTurnStart, ctl.TurnStartData{})
		record(rig.rec)
		writeMessage(t, rig.rec, "steering line")

		require.Eventually(t, func() bool { return strings.Contains(a.out(), "steering line") },
			ctlWait, 5*time.Millisecond)
		assert.NotContains(t, a.out(), "❯ \n")
	})
}

// TestAttachReplayedInjectedDrawsBlock pins the live-append side of the echo
// rule: a replay-marked injected entry appended at rest draws as its own block,
// exactly as the replay of the same branch would, instead of painting nothing.
func TestAttachReplayedInjectedDrawsBlock(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	rig.rec.rec.Message(agent.MessageInfo{
		Message:  llm.Text(llm.RoleUser, "User Ran: echo hi\n\nOutput:\n16:00"),
		Injected: true, Replayed: true,
	})
	writeMessage(t, rig.rec, "real prompt")

	require.Eventually(t, func() bool { return strings.Contains(a.out(), "real prompt") },
		ctlWait, 5*time.Millisecond)
	assert.Contains(t, a.out(), "User Ran: echo hi")
	assert.Contains(t, a.out(), "16:00")
}

// TestAttachSteeredNoteMidTurn pins the note path: a barrier note lands as an
// injected, replay-marked message mid-turn, and its committed entry paints on
// the attached terminal exactly as backfill would draw it — never dropped for
// arriving while the turn streams.
func TestAttachSteeredNoteMidTurn(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	rig.hub.publish(ctl.EventTurnStart, ctl.TurnStartData{}) // this view is mid-turn
	rig.rec.rec.Message(agent.MessageInfo{
		Message:  llm.Text(llm.RoleUser, "Denied with note: test"),
		Injected: true,
		Replayed: true,
	})
	writeMessage(t, rig.rec, "settling prompt") // ordered after the note

	require.Eventually(t, func() bool {
		return strings.Contains(a.out(), "Denied with note: test")
	}, ctlWait, 5*time.Millisecond)
	assert.NotContains(t, a.out(), "❯ \n", "note echoed as a prompt row")
}

// TestAttachErrorResultBody pins the failed-call body: a tool.result error's
// Output commits under its header, like the host terminal commits Content, so
// a denial reason is never lost between the wire shape and the sink.
func TestAttachErrorResultBody(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	rig.hub.publish(ctl.EventToolStart, ctl.ToolStartData{ID: "t1", Name: "bash",
		Label: `bash: echo "expands to: $(echo \"date\")"`})
	rig.hub.publish(ctl.EventToolResult, ctl.ToolResultData{ID: "t1", Name: "bash",
		Output: "denied by user: test", Error: true})

	require.Eventually(t, func() bool {
		return strings.Contains(a.out(), "denied by user: test")
	}, ctlWait, 5*time.Millisecond)
	assert.Contains(t, a.out(), "tool bash failed")
}

// TestAttachViewWatermarkDropsStaleEvents pins the connect-time rule: events
// at or below the backfill watermark are superseded by what was just rendered.
func TestAttachViewWatermarkDropsStaleEvents(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	root, stopRoot := context.WithCancel(t.Context())
	t.Cleanup(stopRoot)
	view := newAttachView(term, nil, root, "")

	stale := ctl.NewEvent(ctl.EventNotice, 5, ctl.NoticeData{Text: "stale echo"})
	fresh := ctl.NewEvent(ctl.EventNotice, 15, ctl.NoticeData{Text: "fresh echo"})
	view.handle(stale)
	view.handle(fresh) // buffered on the actor; the watermark decides which applies
	view.send(func() { assert.NoError(t, view.applyBackfill(ctl.State{}, nil, 10)) })
	view.waitIdle() // queued behind the backfill: proof it finished

	require.Eventually(t, func() bool { return strings.Contains(out(), "fresh echo") },
		ctlWait, 5*time.Millisecond)
	// both notices flush through the same paint path: if fresh landed, a
	// wrongly-applied stale would have landed beside it
	assert.NotContains(t, out(), "stale echo")
}

// TestAttachRedrawKeepsLiveAppends pins the repaint rule: the redraw a head
// move triggers reads the file on the actor, after every append queued ahead of
// it applied, so those entries stay rendered and shown, never wiped by the
// repaint's Reset.
func TestAttachRedrawKeepsLiveAppends(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "recorded prompt")
	})
	a := newAttachRig(t, rig)

	writeMessage(t, rig.rec, "live prompt") // the file write lands before the append event
	require.Eventually(t, func() bool { return strings.Contains(a.out(), "live prompt") },
		ctlWait, 5*time.Millisecond)

	a.view.requestRedraw()
	a.view.waitIdle()

	branch := rigBranch(t, rig.rec)
	require.Len(t, branch, 3) // the session root plus both prompts
	a.view.send(func() {
		for _, e := range branch {
			assert.Contains(t, a.view.rendered, e.ID)
		}
	})
	a.view.waitIdle()
	assert.Contains(t, a.out(), "live prompt")
}

// blockingPick is a local surface whose dialogs return only when their ctx
// ends, standing in for a human mid-dialog. cancelSeen, when non-nil, gets the
// racer's context error at dismissal so tests can assert the dialog closed.
type blockingPick struct {
	mode       tui.Mode
	cancelSeen chan error
}

func (b *blockingPick) Mode() tui.Mode { return b.mode }

func (b *blockingPick) PickContext(ctx context.Context, _ string, _ []tui.PickItem, _ tui.PickOptions) (int, error) {
	<-ctx.Done()
	b.reportCancel(ctx)
	return 0, ctx.Err()
}

func (b *blockingPick) MultiPickContext(_ context.Context, _ string, _ []tui.PickItem, _ tui.MultiPickOptions) ([]int, error) {
	return nil, tui.ErrNoUI
}

func (b *blockingPick) SelectContext(ctx context.Context, _ string, _ []tui.Option) (int, error) {
	<-ctx.Done()
	b.reportCancel(ctx)
	return 0, ctx.Err()
}

// reportCancel publishes the dismissal on cancelSeen without blocking.
func (b *blockingPick) reportCancel(ctx context.Context) {
	if b.cancelSeen == nil {
		return
	}
	select {
	case b.cancelSeen <- ctx.Err():
	default:
	}
}

func (b *blockingPick) ConfirmContext(ctx context.Context, _ string) (bool, error) {
	i, err := b.SelectContext(ctx, "", nil)
	return i == 0, err
}

func (b *blockingPick) InputContext(context.Context, string, string) (string, error) {
	return "", tui.ErrNoUI
}

func (b *blockingPick) InputSecretContext(ctx context.Context, label, placeholder string) (string, error) {
	return b.InputContext(ctx, label, placeholder)
}

func (b *blockingPick) Ask(context.Context, tui.Question) (tui.Answer, error) {
	return tui.Answer{}, tui.ErrNoUI
}

func (b *blockingPick) OpenDecision(tui.DecisionRequest) *tui.Decision {
	return &tui.Decision{}
}

// TestAttachAnswerLocal pins the dialog mapping: each announced kind asks its
// matching local surface and lands on the right resolve shape, a cancelled ctx
// reports no usable answer, an unknown kind shows nothing, and a surface that
// cannot show an approval dialog never answers it with a denial.
func TestAttachAnswerLocal(t *testing.T) {
	// not parallel: the canned surfaces hand out answers in order

	t.Run("shapes", func(t *testing.T) {
		local := &scriptedLocal{mode: tui.ModeAlt, picks: []int{1},
			selects: []int{0}, inputs: []string{"typed answer", "sk"},
			asks: []tui.Answer{{Text: "own words", Chat: true}}}

		t.Run("pick_answers_index", func(t *testing.T) {
			params, ok := answerLocal(t.Context(), local,
				ctl.InteractionPendingData{ID: 1, Kind: "pick", Prompt: "choose",
					Options: []string{"a", "b"}})
			require.True(t, ok)
			require.NotNil(t, params.Index)
			assert.Equal(t, 1, *params.Index)
		})

		t.Run("confirm_answers_select", func(t *testing.T) {
			params, ok := answerLocal(t.Context(), local,
				ctl.InteractionPendingData{ID: 2, Kind: "confirm", Prompt: "sure?",
					Options: []string{"Yes", "No"}})
			require.True(t, ok)
			require.NotNil(t, params.Index)
			assert.Equal(t, 0, *params.Index)
		})

		t.Run("input_answers_text", func(t *testing.T) {
			params, ok := answerLocal(t.Context(), local,
				ctl.InteractionPendingData{ID: 3, Kind: "input", Prompt: "name"})
			require.True(t, ok)
			require.NotNil(t, params.Text)
			assert.Equal(t, "typed answer", *params.Text)
		})

		t.Run("ask_answers_chat", func(t *testing.T) {
			params, ok := answerLocal(t.Context(), local,
				ctl.InteractionPendingData{ID: 4, Kind: "ask", Prompt: "why",
					Options: []string{"one"}})
			require.True(t, ok)
			require.NotNil(t, params.Text)
			assert.Equal(t, "own words", *params.Text)
			assert.True(t, params.Chat)
		})

		t.Run("empty_ask_answers_text", func(t *testing.T) {
			// a no-options ask (the barrier's deny-reason prompt) answered with an
			// empty submit must send text, never the index a refused answer leaves
			// the dialog open behind
			empty := &scriptedLocal{mode: tui.ModeAlt, asks: []tui.Answer{{Text: ""}}}
			params, ok := answerLocal(t.Context(), empty,
				ctl.InteractionPendingData{ID: 6, Kind: "ask", Prompt: "reason for denying"})
			require.True(t, ok)
			require.NotNil(t, params.Text)
			assert.Empty(t, *params.Text)
			assert.Nil(t, params.Index)
		})

		t.Run("optioned_ask_answers_index", func(t *testing.T) {
			choosing := &scriptedLocal{mode: tui.ModeAlt, asks: []tui.Answer{{Index: 1}}}
			params, ok := answerLocal(t.Context(), choosing,
				ctl.InteractionPendingData{ID: 7, Kind: "ask", Prompt: "proceed?",
					Options: []string{"yes", "no"}})
			require.True(t, ok)
			require.NotNil(t, params.Index)
			assert.Equal(t, 1, *params.Index)
		})

		t.Run("secret_answers_hidden", func(t *testing.T) {
			params, ok := answerLocal(t.Context(), local,
				ctl.InteractionPendingData{ID: 5, Kind: "secret", Prompt: "key"})
			require.True(t, ok)
			require.NotNil(t, params.Text)
			assert.Equal(t, "sk", *params.Text)
		})
	})

	t.Run("cancelled_ctx_no_answer", func(t *testing.T) {
		local := &blockingPick{mode: tui.ModeAlt}
		ctx, cancel := context.WithCancel(t.Context())
		cancel() // the settled event's cancellation, arrived first
		_, ok := answerLocal(ctx, local,
			ctl.InteractionPendingData{ID: 9, Kind: "pick", Prompt: "choose",
				Options: []string{"a"}})
		assert.False(t, ok)
	})

	t.Run("unknown_kind_leaves_open", func(t *testing.T) {
		params, ok := answerLocal(t.Context(), &scriptedLocal{mode: tui.ModeAlt},
			ctl.InteractionPendingData{ID: 1, Kind: "mystery", Prompt: "?"})
		assert.False(t, ok)
		assert.Zero(t, params)
	})

	t.Run("decision_without_ui_leaves_open", func(t *testing.T) {
		params, ok := answerLocal(t.Context(), &blockingPick{mode: tui.ModeAlt},
			ctl.InteractionPendingData{ID: 1, Kind: "decision", Prompt: "approve?",
				Subject: "bash: rm -rf", Options: []string{"Allow", "Deny"}})
		assert.False(t, ok)
		assert.Zero(t, params)
	})
}

// TestAttachSnapshotCarriesOpenDialogs pins the resnapshot rule: dialogs are
// runtime state that rides the socket's snapshot, so a controller attaching
// mid-dialog races what holds the run, and a resnapshot cancels racers whose
// dialogs settled while their terminal was away.
func TestAttachSnapshotCarriesOpenDialogs(t *testing.T) {
	// not parallel: the rig pins AJENT_HOME

	t.Run("open_before_attach_races", func(t *testing.T) {
		rig := newServeRig(t, nil) // no host surface: the dialog holds for a remote answer
		answered := make(chan int, 1)
		go func() { // an approval dialog the host terminal cannot answer
			dlg, err := rig.broker.OpenDecision(tui.DecisionRequest{
				Prompt: "allow?", Options: []tui.Option{{Label: "Yes"}, {Label: "No"}},
			})
			assert.NoError(t, err)
			idx, werr := dlg.Wait(t.Context())
			assert.NoError(t, werr)
			answered <- idx
		}()
		require.Eventually(t, func() bool { return len(rig.broker.opened()) == 1 },
			ctlWait, 5*time.Millisecond)

		a := newAttachRig(t, rig) // attaches after the pending event, snapshot only

		var ids []uint64
		a.view.send(func() { // the snapshot's open dialog opened a local racer
			for id := range a.view.dialogs {
				ids = append(ids, id)
			}
		})
		a.view.waitIdle()
		require.Len(t, ids, 1)

		// the raced dialog answers over the socket; its settled event retires the racer
		_, err := a.conn.ClaimSeat(t.Context(), "attach-test", false)
		require.NoError(t, err)
		i := 1
		require.NoError(t, a.conn.Resolve(t.Context(), ctl.ResolveParams{ID: ids[0], Index: &i}))
		assert.Equal(t, 1, <-answered)
		require.Eventually(t, func() bool {
			var racing bool
			a.view.call(func() { _, racing = a.view.dialogs[ids[0]] })
			return !racing
		}, ctlWait, 5*time.Millisecond)
	})

	t.Run("settled_while_away_cancels", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		parent, done := context.WithCancel(t.Context()) // a parent the resnapshot may seed racers from
		t.Cleanup(done)
		a.view.send(func() {
			a.view.dialogs[9] = cancel           // a racer left over from a dropped connection
			a.view.reconcileDialogs(parent, nil) // the resnapshot lists nothing open
			_, racing := a.view.dialogs[9]
			assert.False(t, racing)
			assert.ErrorIs(t, ctx.Err(), context.Canceled)
		})
		a.view.waitIdle()
	})

	t.Run("pending_twice_races_once", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)

		a.view.send(func() {
			d := ctl.InteractionPendingData{ID: 3, Kind: "input", Prompt: "name"}
			a.view.beginInteraction(a.view.root, d)
			a.view.beginInteraction(a.view.root, d) // a resnapshot re-listing the same dialog
			assert.Len(t, a.view.dialogs, 1)
			a.view.settleInteraction(3)
		})
		a.view.waitIdle()
	})
}

// TestAttachSettledCancelsLocalRacer pins the race rule end to end: a settled
// event ends the local dialog and no resolve leaves the process.
func TestAttachSettledCancelsLocalRacer(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	a.view.send(func() { a.view.beginInteraction(a.view.root, ctl.InteractionPendingData{ID: 7}) })
	a.view.send(func() { a.view.dialogs[7] = cancel }) // swap the racer for a controlled one

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.view.raceDialog(ctx, ctl.InteractionPendingData{ID: 7, Kind: "pick",
			Prompt: "choose", Options: []string{"a"}})
	}()
	a.view.send(func() { a.view.settleInteraction(7) }) // the remote side answered first

	<-done
}

// TestAttachPendingRowsPromptOnly pins the pending-row rule: a prompt paints
// its row at submit and keeps it until the entry commits, while slash and
// shell lines never paint one, they surface through their effects.
func TestAttachPendingRowsPromptOnly(t *testing.T) {
	rig := newControlRig(t, nil)
	rig.hub.setSubmitLine(func(string, string, string) error { return nil }) // accepted silently, no entry follows
	a := newAttachRig(t, rig)
	_, err := a.conn.ClaimSeat(t.Context(), "test", false)
	require.NoError(t, err)

	lines := make(chan string, 4)
	var pumped atomic.Bool
	go func() {
		pumpSubmissions(a.view, lines)
		pumped.Store(true)
	}()
	lines <- "/help"
	lines <- "!ls"
	lines <- "a real prompt"
	close(lines)
	require.Eventually(t, pumped.Load, ctlWait, 5*time.Millisecond)

	a.view.send(func() {
		require.Len(t, a.view.pending, 1) // the prompt alone paints one
		assert.Equal(t, "a real prompt", a.view.pending[0].text)
		assert.NotEmpty(t, a.view.pending[0].id) // correlated for drops and commits
	})
	a.view.waitIdle()
}

// mustEncode marshals v, failing the test when it cannot.
func mustEncode(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// TestAttachCommitResolvesQueuedRow pins the row lifecycle: a submitted
// prompt's dimmed row clears from the terminal when its entry commits, whether
// it landed alone or joined with other prompts into one newline-joined message,
// and an annotated @ref resolves through its correlation though the committed
// text no longer matches the typed line.
func TestAttachCommitResolvesQueuedRow(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	committed := func(id string, queueIDs []string, text string) ctl.Event {
		return ctl.NewEvent(ctl.EventEntryAppend, 1, ctl.EntryData{Entry: ctl.Entry{
			ID: id, Type: string(session.TypeMessage),
			Data: mustEncode(t, session.MessageData{
				Message: llm.Text(llm.RoleUser, text), QueueIDs: queueIDs}),
		}})
	}

	t.Run("single_prompt", func(t *testing.T) {
		id := a.view.submitPending("sent prompt")
		a.view.waitIdle()
		assert.Equal(t, []string{"sent prompt"}, a.view.ui.Queued())

		a.view.handle(committed("01RESOLVE1", []string{id}, "sent prompt"))
		a.view.waitIdle()
		assert.Empty(t, a.view.pending)
		assert.Empty(t, a.view.ui.Queued()) // the committed entry took the row's place
	})

	t.Run("joined_run", func(t *testing.T) {
		// queued prompts land as one newline-joined message: both rows resolve
		first := a.view.submitPending("first line")
		second := a.view.submitPending("second line")
		a.view.waitIdle()
		assert.Equal(t, []string{"first line", "second line"}, a.view.ui.Queued())

		a.view.handle(committed("01RESOLVE2", []string{first, second}, "first line\nsecond line"))
		a.view.waitIdle()
		assert.Empty(t, a.view.pending)
		assert.Empty(t, a.view.ui.Queued())
	})

	t.Run("annotated_ref_resolves_by_id", func(t *testing.T) {
		// @-expansion rewrites the committed text away from the typed line: only
		// the correlation resolves the row, the old text match never did
		id := a.view.submitPending("see @notes/big.md")
		a.view.waitIdle()
		assert.Equal(t, []string{"see @notes/big.md"}, a.view.ui.Queued())

		a.view.handle(committed("01RESOLVE3", []string{id}, "see @notes/big.md (800 lines, 64kb)"))
		a.view.waitIdle()
		assert.Empty(t, a.view.pending)
		assert.Empty(t, a.view.ui.Queued())
	})

	t.Run("unrelated_entry_keeps_row", func(t *testing.T) {
		id := a.view.submitPending("still waiting")
		a.view.waitIdle()

		a.view.handle(committed("01RESOLVE4", []string{"another-row"}, "someone else"))
		a.view.waitIdle()
		assert.Len(t, a.view.pending, 1) // no correlation match, the row stays
		assert.Equal(t, []string{"still waiting"}, a.view.ui.Queued())

		a.view.handle(committed("01RESOLVE5", []string{id}, "eventually"))
		a.view.waitIdle()
		assert.Empty(t, a.view.pending)
	})
}

// TestAttachRecallResolvesPending pins the recall rule: a queued prompt the
// host recalled for editing resolves this terminal's pending row through its
// drop event, so the resubmitted replacement never shows beside the original.
func TestAttachRecallResolvesPending(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	id := a.view.submitPending("original")
	a.view.waitIdle()
	assert.Equal(t, []string{"original"}, a.view.ui.Queued())

	// the host queued it (echoing the submit correlation), then recalled it:
	// rows replace and the drop event names the row id
	a.view.handle(ctl.NewEvent(ctl.EventQueued, 1,
		ctl.QueuedData{Rows: []ctl.QueueRow{{ID: id, Text: "original"}}}))
	a.view.waitIdle()
	a.view.handle(ctl.NewEvent(ctl.EventQueueDropped, 2,
		ctl.QueueDroppedData{Rows: []ctl.QueueRow{{ID: id, Text: "original"}}, Cause: "recalled"}))
	a.view.handle(ctl.NewEvent(ctl.EventQueued, 3, ctl.QueuedData{}))
	a.view.waitIdle()
	assert.Empty(t, a.view.pending)

	// the edited resubmission queues as its own row with a fresh correlation,
	// the original gone for good: even the same text never re-resolves it
	edited := a.view.submitPending("original")
	a.view.waitIdle()
	assert.NotEqual(t, id, edited)
	a.view.handle(ctl.NewEvent(ctl.EventQueueDropped, 4,
		ctl.QueueDroppedData{Rows: []ctl.QueueRow{{ID: id}}, Cause: "interrupt"}))
	a.view.waitIdle()
	assert.Equal(t, []string{"original"}, a.view.ui.Queued()) // the fresh row survives
}

// TestAttachSecretRowResolvesByID pins the secret rule end to end: the row's
// text redacts on every wire copy but its id always travels, so an interrupt or
// recall of a queued secret still clears this terminal's pending row — and the
// display keeps the real text from the local submission.
func TestAttachSecretRowResolvesByID(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	id := a.view.submitPending("secret: hunter2")
	a.view.waitIdle()

	// the host queued it; the wire copy drops the text, keeps the id, and this
	// terminal still shows its own typed line
	a.view.handle(ctl.NewEvent(ctl.EventQueued, 1,
		ctl.QueuedData{Rows: []ctl.QueueRow{{ID: id}}}))
	a.view.waitIdle()
	assert.Equal(t, []string{"secret: hunter2"}, a.view.ui.Queued())

	// a foreign secret row shows nothing here and never merges with ours
	a.view.handle(ctl.NewEvent(ctl.EventQueued, 2,
		ctl.QueuedData{Rows: []ctl.QueueRow{{ID: id}, {ID: "row-x"}}}))
	a.view.waitIdle()
	assert.Equal(t, []string{"secret: hunter2"}, a.view.ui.Queued())

	// the interrupt drop names the row by id alone: the pending row resolves
	a.view.handle(ctl.NewEvent(ctl.EventQueueDropped, 3,
		ctl.QueueDroppedData{Rows: []ctl.QueueRow{{ID: id}}, Cause: "interrupt"}))
	a.view.waitIdle()
	assert.Empty(t, a.view.pending)
}

// TestAttachDuplicateTextsResolveIndependently pins the identity rule: two
// identical prompts carry different correlations, so each resolves on its own —
// one commit or one drop never clears its twin.
func TestAttachDuplicateTextsResolveIndependently(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	first := a.view.submitPending("run the same thing")
	second := a.view.submitPending("run the same thing")
	a.view.waitIdle()
	assert.Equal(t, []string{"run the same thing", "run the same thing"}, a.view.ui.Queued())

	// one lands: only its row goes
	a.view.handle(ctl.NewEvent(ctl.EventEntryAppend, 1, ctl.EntryData{Entry: ctl.Entry{
		ID: "01DUP0001", Type: string(session.TypeMessage), Data: mustEncode(t,
			session.MessageData{Message: llm.Text(llm.RoleUser, "run the same thing"), QueueIDs: []string{first}}),
	}}))
	a.view.waitIdle()
	assert.Len(t, a.view.pending, 1)
	assert.Equal(t, []string{"run the same thing"}, a.view.ui.Queued())

	// the other drops by recall: its row goes too, by its own id. The rows
	// update rides beside every drop, like announceRecallLocked publishes it.
	a.view.handle(ctl.NewEvent(ctl.EventQueueDropped, 2,
		ctl.QueueDroppedData{Rows: []ctl.QueueRow{{ID: second}}, Cause: "recalled"}))
	a.view.handle(ctl.NewEvent(ctl.EventQueued, 3, ctl.QueuedData{}))
	a.view.waitIdle()
	assert.Empty(t, a.view.pending)
	assert.Empty(t, a.view.ui.Queued())
}

// TestAttachReconnectResolvesPending pins the resnapshot rule: prompts that
// committed while this terminal was away resolve their pending rows during
// backfill, so a later queued event cannot resurrect them as rows.
func TestAttachReconnectResolvesPending(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	id := a.view.submitPending("offline prompt")
	a.view.waitIdle()

	entry := ctl.Entry{ID: "01OFFLINE1", Type: string(session.TypeMessage), Data: mustEncode(t,
		session.MessageData{Message: llm.Text(llm.RoleUser, "offline prompt"), QueueIDs: []string{id}})}
	snap := ctl.State{Head: entry.ID} // the resnapshot's branch carries the commit
	a.view.send(func() {
		assert.NoError(t, a.view.applyBackfill(snap, []session.Entry{
			{ID: entry.ID, Type: session.TypeMessage, Data: entry.Data}}, 0))
	})
	a.view.waitIdle()
	assert.Empty(t, a.view.pending)

	a.view.handle(ctl.NewEvent(ctl.EventQueued, 5,
		ctl.QueuedData{Rows: []ctl.QueueRow{{ID: "other", Text: "other"}}}))
	a.view.waitIdle()
	assert.Equal(t, []string{"other"}, a.view.ui.Queued()) // no resurrected row
}

// TestAttachAppendDedupeById pins the reconciliation rule: entry.append is
// idempotent by id, a prompt's committed entry resolves this terminal's pending
// row and paints it once, and stream-painted entries mark rendered silently.
func TestAttachAppendDedupeById(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	id := a.view.submitPending("queued prompt")

	entry := ctl.Entry{ID: "01ENTRY01", Type: string(session.TypeMessage), Data: mustEncode(t,
		session.MessageData{Message: llm.Text(llm.RoleUser, "queued prompt"), QueueIDs: []string{id}})}
	a.view.handle(ctl.NewEvent(ctl.EventEntryAppend, 1, ctl.EntryData{Entry: entry}))
	a.view.handle(ctl.NewEvent(ctl.EventEntryAppend, 2, ctl.EntryData{Entry: entry})) // same id again
	a.view.waitIdle()

	// the committed text painted exactly once and the pending row resolved
	require.Eventually(t, func() bool { return strings.Count(a.out(), "queued prompt") == 1 },
		ctlWait, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		done := make(chan struct{})
		empty := false
		a.view.send(func() { empty = len(a.view.pending) == 0; close(done) })
		<-done
		return empty
	}, ctlWait, 5*time.Millisecond)
	a.view.send(func() { assert.Contains(t, a.view.rendered, entry.ID) })
	a.view.waitIdle()

	// an assistant entry while a turn streams paints nothing: the stream owns it
	assistant := ctl.Entry{ID: "01ENTRY02", Type: string(session.TypeMessage),
		Data: mustEncode(t, session.MessageData{Message: llm.Text(llm.RoleAssistant, "streamed reply")})}
	a.view.handle(ctl.NewEvent(ctl.EventTurnStart, 3, ctl.TurnStartData{Model: "p/m"}))
	a.view.handle(ctl.NewEvent(ctl.EventEntryAppend, 4, ctl.EntryData{Entry: assistant}))
	a.view.waitIdle()
	require.Eventually(t, func() bool { return len(a.view.rendered) >= 3 }, ctlWait, 5*time.Millisecond)
	assert.NotContains(t, a.out(), "streamed reply")
	a.view.send(func() { assert.Contains(t, a.view.rendered, assistant.ID) })
	a.view.waitIdle()
}

// TestAttachReconnectClearsStaleChrome pins the resnapshot reconcile against a
// mid-turn disconnect: segments, activity rows and queue rows the new snapshot
// no longer lists clear instead of lingering, and a spinner whose turn ended
// while this terminal was away does not survive the reconnect.
func TestAttachReconnectClearsStaleChrome(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	// chrome paints live over the wire, then this surface goes mid-turn with no
	// end event following it
	rig.hub.ui.SetStatusSegment(segment(segMCP, "mcp 1/2", ""))
	rig.hub.ui.SetActivityRanked("job:1", "reading", 1)
	rig.hub.ui.SetQueued([]ctl.QueueRow{{ID: "q1", Text: "queued prompt"}})
	a.view.handle(ctl.NewEvent(ctl.EventTurnStart, 10, ctl.TurnStartData{}))
	require.Eventually(t, func() bool {
		var ok bool
		ran := a.view.call(func() {
			ok = len(a.view.ap.segs) == 1 && len(a.view.ap.acts) == 1 &&
				len(a.view.wireQueued) == 1 && a.view.running
		})
		return ran && ok
	}, ctlWait, 5*time.Millisecond)
	require.False(t, a.view.ui.Idle())

	// the reconnect's snapshot lists none of it: all of it clears
	snap := ctl.State{Head: rig.rec.w.Head(), Idle: true}
	a.view.send(func() { require.NoError(t, a.view.applyBackfill(snap, nil, 0)) })
	a.view.waitIdle()

	assert.Empty(t, a.view.ui.Queued())
	assert.True(t, a.view.ui.Idle())
	a.view.send(func() {
		assert.False(t, a.view.running) // the forced turn end recovered the idle fact
		assert.Empty(t, a.view.ap.segs)
		assert.Empty(t, a.view.ap.acts)
	})
	a.view.waitIdle()
}
