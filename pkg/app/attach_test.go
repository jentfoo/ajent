package app

import (
	"context"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
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
	return newAttachRigTerm(t, rig, term, out)
}

// newAttachRigTerm is newAttachRig over one chosen terminal, so picker tests
// can drive the attach surface's own input.
func newAttachRigTerm(t *testing.T, rig *controlRig, term *tui.UI, out func() string) *attachRig {
	t.Helper()

	conn, hello, err := client.Dial(rig.path)
	require.NoError(t, err)
	t.Cleanup(conn.Close)
	assert.Equal(t, ctl.Protocol, hello.Protocol)

	root, stopRoot := context.WithCancel(context.Background())
	t.Cleanup(stopRoot)
	view := newAttachView(term, conn, root, hello.TranscriptPath)
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

// rigBranch reads the recorded transcript's current branch, oldest first: the
// same file path hello names, which is what the view itself reads.
func rigBranch(t *testing.T, rec *sessRec) []session.Entry {
	t.Helper()
	entries, _, err := session.Read(rec.w.Path())
	require.NoError(t, err)
	return session.Branch(entries, rec.w.Head())
}

// TestAttachPromptsFromFile pins the recall source: the attached session's
// recorded user prompts arrive newest first, and injected context, assistant
// messages and secret-prefixed lines never surface.
func TestAttachPromptsFromFile(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		rec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "secret: hunter2")})
		rec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleAssistant, "an answer")})
		rec.rec.Message(agent.MessageInfo{Message: llm.Text(llm.RoleUser, "injected"), Injected: true})
		writeMessage(t, rec, "second prompt")
	})
	a := newAttachRig(t, rig)

	ps := a.view.prompts()
	assert.Equal(t, []string{"second prompt", "first prompt"}, session.PromptTexts(ps))
}

// TestAttachPromptsFollowHead pins the freshness rule: prompts submitted after
// attach join the recall list once their entry.append moves the head.
func TestAttachPromptsFollowHead(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "before attach")
	})
	a := newAttachRig(t, rig)

	ps := a.view.prompts()
	require.Len(t, ps, 1)
	assert.Equal(t, "before attach", ps[0].Text)

	writeMessage(t, rig.rec, "after attach")
	require.Eventually(t, func() bool {
		return len(a.view.prompts()) == 2
	}, ctlWait, 5*time.Millisecond)
}

// TestAttachRewindPickerForksRemote pins the double-Esc flow over the file:
// the picker reads this tree from the recorded transcript and a picked prompt
// rewinds the remote session onto its parent, forked away like a local rewind.
func TestAttachRewindPickerForksRemote(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
	})
	wireAttachRewind(t, rig)

	term, feed, out := pipeInputUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	_, err := a.conn.ClaimSeat(ctx, "test", false)
	require.NoError(t, err)

	branch := rigBranch(t, rig.rec)
	rowID := branch[len(branch)-1].ID // the newest recorded prompt is pickable last
	wantHead, fill, ok := session.RewindTarget(branch, rowID)
	require.True(t, ok)
	assert.Equal(t, "second prompt", fill)

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.view.rewindPicker()
	}()
	feed("second\n") // the pick overlay reads one line as its filter in plain mode
	select {
	case <-done:
	case <-time.After(ctlWait):
		t.Fatal("rewind picker did not complete")
	}
	assert.Equal(t, wantHead, rig.rec.w.Head()) // the remote session forked onto it
}

// TestAttachRewindObserverRefused pins the refusal path: a rewind from a
// connection without the control seat moves nothing.
func TestAttachRewindObserverRefused(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
	})
	wireAttachRewind(t, rig)

	a := newAttachRig(t, rig) // no seat claimed: an observer

	branch := rigBranch(t, rig.rec)
	rowID := branch[len(branch)-1].ID

	require.Error(t, a.view.rewindToRow(branch, rowID))
	assert.Equal(t, rig.rec.w.Head(), branch[len(branch)-1].ID) // nothing moved
}

// TestAttachRewindFillsEditor pins the double-Esc tail: rewinding pre-fills
// this editor with the picked prompt so it can be edited or re-sent as the
// restored branch's first message, exactly like a local rewind.
func TestAttachRewindFillsEditor(t *testing.T) {
	rig := newControlRig(t, func(rec *sessRec) {
		writeMessage(t, rec, "first prompt")
		writeMessage(t, rec, "second prompt")
	})
	wireAttachRewind(t, rig)

	term, feed, out := ptyUI(t)
	a := newAttachRigTerm(t, rig, term, out)

	ctx, cancel := context.WithTimeout(context.Background(), client.CallWait)
	defer cancel()
	_, err := a.conn.ClaimSeat(ctx, "test", false)
	require.NoError(t, err)

	branch := rigBranch(t, rig.rec)

	// the fill paints the editor into the live block...
	require.NoError(t, a.view.rewindToRow(branch, branch[len(branch)-1].ID))
	require.Eventually(t, func() bool { return strings.Contains(out(), "second prompt") },
		ctlWait, 5*time.Millisecond)
	// ...and submitting proves what the buffer held
	feed("\r")
	select {
	case line := <-term.Messages():
		assert.Equal(t, "second prompt", line)
	case <-time.After(ctlWait):
		t.Fatal("rewind did not pre-fill the editor")
	}
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
	view := newAttachView(term, nil, root, "")

	stale := ctl.NewEvent(ctl.EventEcho, 5, ctl.EchoData{Text: "stale echo"})
	fresh := ctl.NewEvent(ctl.EventEcho, 15, ctl.EchoData{Text: "fresh echo"})
	view.handle(stale)
	view.handle(fresh) // buffered too; the watermark decides which one applies
	view.applyBackfill(ctl.State{}, nil, 10)

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

// pipeInputUI opens a plain-mode TUI over pipes like pipeUI, also returning
// the input writer so tests can drive dialogs that read one line.
func pipeInputUI(t *testing.T) (*tui.UI, func(string), func() string) {
	t.Helper()

	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})
	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	var mu sync.Mutex
	var sb strings.Builder
	go func() { _, _ = io.Copy(&lockedWriter{mu: &mu, sb: &sb}, outR) }()
	return ui, func(data string) { _, _ = io.WriteString(inW, data) }, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// ptyUI runs one inline-mode terminal over a real pty: the returned writer
// feeds keys and submits land on Messages. Plain mode has no key loop, which
// hides editor fills; TERM must name a real terminal or ResolveMode downgrades
// to plain before anything can be observed.
func ptyUI(t *testing.T) (*tui.UI, func(string), func() string) {
	t.Helper()
	t.Setenv("TERM", "xterm")

	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = master.Close() })

	var unlock int32 // TIOCSPTLCK: zero unlocks the slave
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK,
		uintptr(unsafe.Pointer(&unlock)))
	require.Zero(t, errno)

	var ptyNo int32
	_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN,
		uintptr(unsafe.Pointer(&ptyNo)))
	require.Zero(t, errno)

	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(int(ptyNo)), os.O_RDWR, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = slave.Close() })

	ui, err := tui.New(tui.Options{In: slave, Out: slave, Mode: tui.ModeInline})
	require.NoError(t, err)
	t.Cleanup(ui.Close)

	var mu sync.Mutex
	var sb strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, rerr := master.Read(buf) // blocks until the terminal paints or closes
			if n > 0 {
				mu.Lock()
				sb.Write(buf[:n])
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	return ui, func(keys string) { _, _ = master.WriteString(keys) }, func() string {
		mu.Lock()
		defer mu.Unlock()
		return sb.String()
	}
}

// wireAttachRewind arms the rewind seam over one rig with a live agent and
// registry, so session.rewind requests rebuild the branch like the driver's.
func wireAttachRewind(t *testing.T, rig *controlRig) {
	t.Helper()

	reg, warnings := llm.NewRegistry(llm.File{
		Providers: map[string]llm.ProviderConfig{"p": {Models: []llm.ModelConfig{{ID: "a"}}}},
	}, nil, llm.RegistryOptions{})
	require.Empty(t, warnings)
	st := &agent.State{Model: reg.Active(), Tokens: tokens.New(reg.Active())}
	ag := agent.New(st, agent.Options{
		Provider: func(llm.Model) (llm.Provider, error) { return &llm.ScriptedProvider{}, nil },
		Sinks:    []agent.Sink{rig.rec.rec.Sink(rig.hub.sink())},
		Env:      agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
	})
	rig.hub.ag = ag
	rig.hub.syncLedger()
	rig.hub.rewindTo = func(head string) (string, error) {
		return rig.rec.rewindTo(rig.hub.ui, ag, reg, head)
	}
}
