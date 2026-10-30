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
	select {
	case line := <-term.Messages():
		assert.Equal(t, "searchable prompt", line)
	case <-time.After(ctlWait):
		t.Fatal("history search did not fill the editor")
	}
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
		assert.NotContains(t, a.out(), "❯ \n", "tool-result entry echoed as an empty prompt row")
	})

	t.Run("while_streaming", func(t *testing.T) {
		rig := newControlRig(t, nil)
		a := newAttachRig(t, rig)
		rig.hub.publish(ctl.EventTurnStart, ctl.TurnStartData{})
		record(rig.rec)
		writeMessage(t, rig.rec, "steering line")

		require.Eventually(t, func() bool { return strings.Contains(a.out(), "steering line") },
			ctlWait, 5*time.Millisecond)
		assert.NotContains(t, a.out(), "❯ \n", "tool-result entry echoed as an empty prompt row")
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
	assert.Contains(t, a.out(), "User Ran: echo hi", "replayed injected block missing from live append")
	assert.Contains(t, a.out(), "16:00", "replayed injected body missing from live append")
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
		require.Len(t, ids, 1, "open dialog did not race from the snapshot")

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
			assert.False(t, racing, "racer outlived its dialog")
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

	select {
	case <-done:
	case <-time.After(ctlWait):
		t.Fatal("settled did not cancel the local racer")
	}
}

// TestAttachPendingRowsPromptOnly pins the pending-row rule: a prompt paints
// its row at submit and keeps it until the entry commits, while slash and
// shell lines never paint one, they surface through their effects.
func TestAttachPendingRowsPromptOnly(t *testing.T) {
	rig := newControlRig(t, nil)
	rig.hub.setSubmitLine(func(string, string) error { return nil }) // accepted silently, no entry follows
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
		assert.Equal(t, []string{"a real prompt"}, a.view.pending)
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
// it landed alone or joined with other prompts into one newline-joined message.
func TestAttachCommitResolvesQueuedRow(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	committed := func(id, text string) ctl.Event {
		return ctl.NewEvent(ctl.EventEntryAppend, 1, ctl.EntryData{Entry: ctl.Entry{
			ID: id, Type: string(session.TypeMessage),
			Data: mustEncode(t, session.MessageData{Message: llm.Text(llm.RoleUser, text)}),
		}})
	}

	t.Run("single_prompt", func(t *testing.T) {
		a.view.submitPending("sent prompt")
		a.view.waitIdle()
		assert.Equal(t, []string{"sent prompt"}, a.view.ui.Queued())

		a.view.handle(committed("01RESOLVE1", "sent prompt"))
		a.view.waitIdle()
		assert.Empty(t, a.view.pending)
		assert.Empty(t, a.view.ui.Queued()) // the committed entry took the row's place
	})

	t.Run("joined_run", func(t *testing.T) {
		// queued prompts land as one newline-joined message: both rows resolve
		a.view.submitPending("first line")
		a.view.submitPending("second line")
		a.view.waitIdle()
		assert.Equal(t, []string{"first line", "second line"}, a.view.ui.Queued())

		a.view.handle(committed("01RESOLVE2", "first line\nsecond line"))
		a.view.waitIdle()
		assert.Empty(t, a.view.pending)
		assert.Empty(t, a.view.ui.Queued())
	})
}

// TestAttachReconnectResolvesPending pins the resnapshot rule: prompts that
// committed while this terminal was away resolve their pending rows during
// backfill, so a later queued event cannot resurrect them as rows.
func TestAttachReconnectResolvesPending(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	a.view.submitPending("offline prompt")
	a.view.waitIdle()

	entry := ctl.Entry{ID: "01OFFLINE1", Type: string(session.TypeMessage),
		Data: mustEncode(t, session.MessageData{Message: llm.Text(llm.RoleUser, "offline prompt")})}
	snap := ctl.State{Head: entry.ID} // the resnapshot's branch carries the commit
	a.view.send(func() {
		assert.NoError(t, a.view.applyBackfill(snap, []session.Entry{
			{ID: entry.ID, Type: session.TypeMessage, Data: entry.Data}}, 0))
	})
	a.view.waitIdle()
	assert.Empty(t, a.view.pending)

	a.view.handle(ctl.NewEvent(ctl.EventQueued, 5, ctl.QueuedData{Texts: []string{"other"}}))
	a.view.waitIdle()
	assert.Equal(t, []string{"other"}, a.view.ui.Queued()) // no resurrected row
}

// TestAttachAppendDedupeById pins the reconciliation rule: entry.append is
// idempotent by id, a prompt's committed entry resolves this terminal's pending
// row and paints it once, and stream-painted entries mark rendered silently.
func TestAttachAppendDedupeById(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)
	a.view.submitPending("queued prompt")

	entry := ctl.Entry{ID: "01ENTRY01", Type: string(session.TypeMessage),
		Data: mustEncode(t, session.MessageData{Message: llm.Text(llm.RoleUser, "queued prompt")})}
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
