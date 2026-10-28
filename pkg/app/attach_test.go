package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/tui"
)

// attachRig pairs a control rig with one attached client view: the socket is
// live, events flow through the view's handler, and the terminal output is
// captured for assertions.
type attachRig struct {
	rig    *controlRig
	conn   *client.Conn
	view   *attachView
	out    func() string
	events chan ctl.Event

	mu      sync.Mutex
	handled []ctl.Event
}

// newAttachRig attaches a fresh plain-mode terminal to rig and completes its
// backfill, so tests assert against rendered history plus live events.
func newAttachRig(t *testing.T, rig *controlRig) *attachRig {
	t.Helper()

	term, out := pipeUI(t)
	conn, hello, err := client.Dial(rig.path)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Equal(t, ctl.Protocol, hello.Protocol)

	root, stopRoot := context.WithCancel(context.Background())
	t.Cleanup(stopRoot)
	view := newAttachView(term, conn, root)
	a := &attachRig{rig: rig, conn: conn, view: view, out: out,
		events: make(chan ctl.Event, 512)}
	go conn.Serve(func(ev ctl.Event) {
		a.mu.Lock()
		a.handled = append(a.handled, ev)
		a.mu.Unlock()
		select {
		case a.events <- ev:
		default: // a slow reader never blocks delivery
		}
		view.handle(ev)
	})

	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	require.NoError(t, view.backfill(ctx))
	return a
}

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

// TestAttachViewStreamsLiveEvents pins surface parity for committed history:
// an echo, keyed and unkeyed notices, and /command print output each reach the
// attached terminal as committed. Status-line chrome (segments, queued rows,
// activity) forwards through the same tui setters the host uses; its rendering
// is pkg/tui's own tested behavior, not capturable in plain scrollback.
func TestAttachViewStreamsLiveEvents(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	rig.hub.ui.UserEcho("typed on host")
	rig.hub.ui.NotifyKeyed("subagent", "one job", tui.LevelInfo) // parked until the next commit flushes it
	rig.hub.ui.Print("## printed block")
	rig.hub.ui.Notify("plain notice", tui.LevelWarn)

	require.Eventually(t, func() bool {
		out := a.out()
		return strings.Contains(out, "typed on host") &&
			strings.Contains(out, "one job") &&
			strings.Contains(out, "printed block") &&
			strings.Contains(out, "plain notice")
	}, ctlWait, 5*time.Millisecond)
}

// TestAttachViewStreamsTurnText pins the stream mapping: text deltas render as
// they arrive and end committed, like the host's own renderer shows them.
func TestAttachViewStreamsTurnText(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	rig.hub.publish(ctl.EventTextDelta, ctl.TextData{Delta: "partial "})
	rig.hub.publish(ctl.EventTextDelta, ctl.TextData{Delta: "answer"})
	rig.hub.publish(ctl.EventTextEnd, nil)

	require.Eventually(t, func() bool { return strings.Contains(a.out(), "partial answer") },
		ctlWait, 5*time.Millisecond)
}

// TestAttachViewToolHeaderAndResult pins the tool mapping: a start renders its
// header, streamed output lands under it and the result commits the body.
func TestAttachViewToolHeaderAndResult(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	rig.hub.publish(ctl.EventToolStart, ctl.ToolStartData{ID: "t1", Name: "bash", Label: "bash: ls"})
	rig.hub.publish(ctl.EventToolOutput, ctl.ToolOutputData{ID: "t1", Delta: "file.go\n"})
	rig.hub.publish(ctl.EventToolResult, ctl.ToolResultData{ID: "t1", Name: "bash"})

	require.Eventually(t, func() bool {
		out := a.out()
		return strings.Contains(out, "bash: ls") && strings.Contains(out, "file.go")
	}, ctlWait, 5*time.Millisecond)
}

// TestAttachViewToolStreamsOnce pins the no-duplicate rule: a tool whose output
// streamed locally commits its body exactly once — the full-text copy riding
// the wire result is for controllers without the stream, and this surface must
// not render it again.
func TestAttachViewToolStreamsOnce(t *testing.T) {
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
}

// TestAttachViewNonStreamingResultRenders pins the Display treatment: a tool
// that never streamed shows its result body exactly once.
func TestAttachViewNonStreamingResultRenders(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	rig.hub.publish(ctl.EventToolStart, ctl.ToolStartData{ID: "t2", Name: "write"})
	rig.hub.publish(ctl.EventToolResult, ctl.ToolResultData{ID: "t2", Name: "write",
		Output: "wrote notes.go"})

	require.Eventually(t, func() bool {
		return strings.Count(a.out(), "wrote notes.go") == 1
	}, ctlWait, 5*time.Millisecond)
}

// TestAttachViewWatermarkDropsStaleEvents pins the connect-time rule: events
// at or below the backfill watermark are superseded by what was just rendered.
func TestAttachViewWatermarkDropsStaleEvents(t *testing.T) {
	t.Parallel()

	term, out := pipeUI(t)
	root, stopRoot := context.WithCancel(context.Background())
	defer stopRoot()
	view := newAttachView(term, nil, root)

	stale := ctl.NewEvent(ctl.EventEcho, 5, ctl.EchoData{Text: "stale echo"})
	fresh := ctl.NewEvent(ctl.EventEcho, 15, ctl.EchoData{Text: "fresh echo"})
	view.handle(stale)
	view.handle(fresh) // buffered too; the watermark decides which one applies
	view.applyBackfill(ctl.State{}, ctl.TranscriptResult{}, 10)

	require.Eventually(t, func() bool { return strings.Contains(out(), "fresh echo") },
		2*time.Second, 5*time.Millisecond)
	// both echoes would flush through the same paint path: if fresh landed, a
	// wrongly-applied stale would have landed beside it
	assert.NotContains(t, out(), "stale echo")
}

// TestAttachSubmitRefusedWithoutRouter pins the legible refusal: submitting to
// a run still wiring reports unavailable, never a silent drop.
func TestAttachSubmitRefusedWithoutRouter(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	_, err := a.conn.ClaimSeat(ctx, "test", false)
	require.NoError(t, err)
	err = a.conn.Submit(ctx, "hello")
	require.Error(t, err)
	var wire *ctl.Error
	require.ErrorAs(t, err, &wire)
	assert.Equal(t, ctl.CodeUnavailable, wire.Code)
}

// blockingPick is a local surface whose dialogs return only when their ctx
// ends, standing in for a human mid-dialog.
type blockingPick struct {
	mode tui.Mode
}

func (b *blockingPick) Mode() tui.Mode { return b.mode }

func (b *blockingPick) PickContext(ctx context.Context, _ string, _ []tui.PickItem, _ tui.PickOptions) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func (b *blockingPick) MultiPickContext(_ context.Context, _ string, _ []tui.PickItem, _ tui.MultiPickOptions) ([]int, error) {
	return nil, tui.ErrNoUI
}

func (b *blockingPick) SelectContext(ctx context.Context, _ string, _ []tui.Option) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
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

// TestAttachAnswerLocalShapes pins the dialog mapping: each announced kind asks
// its matching local surface and lands on the right resolve shape.
func TestAttachAnswerLocalShapes(t *testing.T) {
	t.Parallel()

	local := &scriptedLocal{mode: tui.ModeAlt, picks: []int{1},
		selects: []int{0}, inputs: []string{"typed answer", "sk"},
		asks: []tui.Answer{{Text: "own words", Chat: true}}}

	pickParams, ok := answerLocal(context.Background(), local,
		ctl.InteractionPendingData{ID: 1, Kind: "pick", Prompt: "choose",
			Options: []string{"a", "b"}})
	require.True(t, ok)
	require.NotNil(t, pickParams.Index)
	assert.Equal(t, 1, *pickParams.Index)

	confirmParams, ok := answerLocal(context.Background(), local,
		ctl.InteractionPendingData{ID: 2, Kind: "confirm", Prompt: "sure?",
			Options: []string{"Yes", "No"}})
	require.True(t, ok)
	require.NotNil(t, confirmParams.Index)
	assert.Equal(t, 0, *confirmParams.Index)

	inputParams, ok := answerLocal(context.Background(), local,
		ctl.InteractionPendingData{ID: 3, Kind: "input", Prompt: "name"})
	require.True(t, ok)
	require.NotNil(t, inputParams.Text)
	assert.Equal(t, "typed answer", *inputParams.Text)

	askParams, ok := answerLocal(context.Background(), local,
		ctl.InteractionPendingData{ID: 4, Kind: "ask", Prompt: "why",
			Options: []string{"one"}})
	require.True(t, ok)
	require.NotNil(t, askParams.Text)
	assert.Equal(t, "own words", *askParams.Text)
	assert.True(t, askParams.Chat)

	secretParams, ok := answerLocal(context.Background(), local,
		ctl.InteractionPendingData{ID: 5, Kind: "secret", Prompt: "key"})
	require.True(t, ok)
	require.NotNil(t, secretParams.Text)
	assert.Equal(t, "sk", *secretParams.Text)
}

// TestAttachAnswerLocalCancelled pins the race rule inside answerLocal: when
// the ctx was cancelled first (a settled event arrived), the racer reports no
// usable answer and raceDialog sends nothing.
func TestAttachAnswerLocalCancelled(t *testing.T) {
	t.Parallel()

	local := &blockingPick{mode: tui.ModeAlt}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the settled event's cancellation, arrived first
	_, ok := answerLocal(ctx, local,
		ctl.InteractionPendingData{ID: 9, Kind: "pick", Prompt: "choose",
			Options: []string{"a"}})
	assert.False(t, ok)
}

// TestAttachSettledCancelsLocalRacer pins the race rule end to end: a settled
// event ends the local dialog and no resolve leaves the process.
func TestAttachSettledCancelsLocalRacer(t *testing.T) {
	rig := newControlRig(t, nil)
	a := newAttachRig(t, rig)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.view.registerDialog(7, cancel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.view.raceDialog(ctx, ctl.InteractionPendingData{ID: 7, Kind: "pick",
			Prompt: "choose", Options: []string{"a"}})
	}()
	a.view.settleInteraction(7) // the remote side answered first

	select {
	case <-done:
	case <-time.After(ctlWait):
		t.Fatal("settled did not cancel the local racer")
	}
}

// TestAttachUnknownKindLeavesDialogOpen pins the refusal that keeps a remote
// dialog honest: an unknown kind shows nothing and answers nothing.
func TestAttachUnknownKindLeavesDialogOpen(t *testing.T) {
	t.Parallel()

	params, ok := answerLocal(context.Background(), &scriptedLocal{mode: tui.ModeAlt},
		ctl.InteractionPendingData{ID: 1, Kind: "mystery", Prompt: "?"})
	assert.False(t, ok)
	assert.Zero(t, params)
}

// TestAttachDecisionNoUILeavesDialogOpen pins the barrier rule: a surface that
// cannot show an approval dialog never answers it with a denial.
func TestAttachDecisionNoUILeavesDialogOpen(t *testing.T) {
	t.Parallel()

	params, ok := answerLocal(context.Background(), &blockingPick{mode: tui.ModeAlt},
		ctl.InteractionPendingData{ID: 1, Kind: "decision", Prompt: "approve?",
			Subject: "bash: rm -rf", Options: []string{"Allow", "Deny"}})
	assert.False(t, ok)
	assert.Zero(t, params)
}
