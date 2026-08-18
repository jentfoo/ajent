package permit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDialog is a controllable approval dialog for tests.
type fakeDialog struct {
	ch       chan int // answers delivered by Resolve or an explicit answer
	resolved sync.Once
}

func newFakeDialog() *fakeDialog { return &fakeDialog{ch: make(chan int, 1)} }

// answer settles the dialog as if a keystroke chose index.
func (d *fakeDialog) answer(idx int) { d.Resolve(idx) }

func (d *fakeDialog) Wait(ctx context.Context) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case idx := <-d.ch:
		return idx, nil
	}
}

// Resolve settles the dialog from a caller; only the first wins.
func (d *fakeDialog) Resolve(index int) {
	d.resolved.Do(func() { d.ch <- index })
}

func (d *fakeDialog) Close() {}

// fakePrompter records dialogs and answers them via answerCh on demand.
type fakePrompter struct {
	mu      sync.Mutex
	err     error // when set, Open returns it before any dialog
	dialogs []*fakeDialog
	reasons map[string]string // label -> canned reason for Reason()
}

func newFakePrompter() *fakePrompter { return &fakePrompter{reasons: make(map[string]string)} }

func (f *fakePrompter) Open(prompt, subject string, options []string) (Dialog, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	d := newFakeDialog()
	f.dialogs = append(f.dialogs, d)
	return d, nil
}

func (f *fakePrompter) Reason(ctx context.Context, label string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.reasons[label]
	if !ok {
		return "", false
	}
	delete(f.reasons, label)
	return r, true
}

func (f *fakePrompter) last() *fakeDialog {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.dialogs) == 0 {
		return nil
	}
	return f.dialogs[len(f.dialogs)-1]
}

func (f *fakePrompter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dialogs)
}

// waitDialog blocks until the prompter has opened a dialog, then returns it. The
// asker runs in its own goroutine during tests, so the Open may lag the call.
func waitDialog(t *testing.T, p *fakePrompter) *fakeDialog {
	t.Helper()
	require.Eventually(t, func() bool { return p.last() != nil }, time.Second, 10*time.Millisecond)
	return p.last()
}

// runAndAnswer drives the asker in a goroutine, answers its dialog with idx, and
// returns the resulting decision.
func runAndAnswer(t *testing.T, p *fakePrompter, b *Barrier, name string, input []byte, idx int) tools.Decision {
	t.Helper()
	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), name, input); close(done) }()
	waitDialog(t, p).answer(idx)
	<-done
	return got
}

// fakeClassifier returns a canned verdict, optionally blocking until ctx ends.
type fakeClassifier struct {
	verdict Class
	block   bool // when true, waits for ctx cancellation and reports unsure
	calls   int  // invocation count (guarded by the single asker goroutine)
}

func (c *fakeClassifier) Classify(ctx context.Context, command string) Class {
	if c.block {
		<-ctx.Done()
		return ClassUnsure
	}
	c.calls++
	return c.verdict
}

// fakeNoter records injected notes.
type fakeNoter struct {
	mu    sync.Mutex
	notes []string
}

func (n *fakeNoter) Note(s string) { n.mu.Lock(); defer n.mu.Unlock(); n.notes = append(n.notes, s) }
func (n *fakeNoter) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.notes...)
}

func newTestBarrier(p *fakePrompter) *Barrier {
	b := NewBarrier(noRO)
	if p != nil {
		b.SetPrompter(p)
	}
	return b
}

// runAsk drives the asker for one call and returns its decision.
func runAsk(b *Barrier, ctx context.Context, name string, input []byte) tools.Decision {
	ask := b.Asker()
	return ask(ctx, agent.ToolCall{ID: "c", Name: name, Input: input}, tools.Decision{Action: tools.ActionAsk})
}

func TestGuardModeMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode Mode
		call agent.ToolCall
		want tools.Action // read-only, write, rejected and unverifiable per mode
	}{
		// allow-all runs everything.
		{"allow-all readonly", ModeAllowAll, bashCall("ls -la"), tools.ActionAllow},
		{"allow-all write", ModeAllowAll, call("write", `{}`), tools.ActionAllow},
		{"allow-all reject", ModeAllowAll, bashCall(`sed -i s/a/b/ f`), tools.ActionAllow},

		// allow-read runs reads, asks writes and unverifiable, denies sed -i.
		{"read readonly", ModeAllowRead, bashCall("ls -la"), tools.ActionAllow},
		{"read write", ModeAllowRead, call("write", `{}`), tools.ActionAsk},
		{"read reject", ModeAllowRead, bashCall(`sed -i s/a/b/ f`), tools.ActionDeny},
		{"read unverifiable", ModeAllowRead, bashCall("stat f"), tools.ActionAsk},

		// block-all asks reads and writes alike; sed stays a hard deny.
		{"block readonly", ModeBlockAll, bashCall("ls -la"), tools.ActionAsk},
		{"block write", ModeBlockAll, call("write", `{}`), tools.ActionAsk},
		{"block reject", ModeBlockAll, bashCall(`sed -i s/a/b/ f`), tools.ActionDeny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBarrier(newFakePrompter())
			b.SetMode(c.mode)
			d := b.Guard()(context.Background(), c.call)
			assert.Equal(t, c.want, d.Action)
		})
	}
}

func TestGuardSafeCommandsOverridePromptButNotRejectOrBlockAll(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	// a bash line and an MCP tool name; write/edit can never be listed.
	b.SetSafeCommands([]string{"git status", "mcp__list", "write"})

	cases := []struct {
		name string
		mode Mode
		call agent.ToolCall
		want tools.Action
	}{
		// an unverifiable bash line named verbatim auto-runs in allow-read/auto.
		{"safe bash read", ModeAllowRead, bashCall("git status"), tools.ActionAllow},
		{"safe bash auto", ModeAuto, bashCall("git status"), tools.ActionAllow},
		// whitespace is tolerated; a different command still prompts.
		{"safe bash padded", ModeAllowRead, bashCall("  git status "), tools.ActionAllow},
		{"unsafe bash prompt", ModeAllowRead, bashCall("git push"), tools.ActionAsk},
		// an exact MCP tool name auto-runs without needing registry metadata.
		{"safe mcp tool", ModeAllowRead, call("mcp__list", `{}`), tools.ActionAllow},
		{"other mcp prompt", ModeAllowRead, call("mcp__write", `{}`), tools.ActionAsk},
		// block-all still asks even a listed safe command.
		{"safe bash block", ModeBlockAll, bashCall("git status"), tools.ActionAsk},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b.SetMode(c.mode)
			d := b.Guard()(context.Background(), c.call)
			assert.Equal(t, c.want, d.Action)
		})
	}

	// a config entry can never un-reject an in-place write.
	b2 := newTestBarrier(newFakePrompter())
	b2.SetSafeCommands([]string{"sed -i s/a/b/ f"})
	d := b2.Guard()(context.Background(), bashCall("sed -i s/a/b/ f"))
	assert.Equal(t, tools.ActionDeny, d.Action)

	// write/edit are excluded from safe matching by name.
	b3 := newTestBarrier(newFakePrompter())
	b3.SetSafeCommands([]string{"write", "edit"})
	assert.Equal(t, tools.ActionAsk, b3.Guard()(context.Background(), call("write", `{}`)).Action)
	assert.Equal(t, tools.ActionAsk, b3.Guard()(context.Background(), call("edit", `{"path":"x.go","edits":[]}`)).Action)
}

func TestGuardDeniedCommandsRefuseWithoutPrompting(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	// a bash line and an MCP tool name; a core writer may also be denied.
	b.SetDeniedCommands([]string{"git stash", "mcp__danger", "write"})

	cases := []struct {
		name string
		mode Mode
		call agent.ToolCall
		want tools.Action
	}{
		// a configured bash prefix denies in every mode, even allow-all.
		{"deny git stash allow all", ModeAllowAll, bashCall("git stash"), tools.ActionDeny},
		{"deny git stash subcommand", ModeAuto, bashCall("git stash push -m x"), tools.ActionDeny},
		// whitespace is tolerated; a different command still asks.
		{"deny padded", ModeAllowRead, bashCall("  git stash "), tools.ActionDeny},
		{"unlisted bash prompts", ModeAllowRead, bashCall("git push"), tools.ActionAsk},
		// an exact tool name denies regardless of registry metadata.
		{"deny mcp tool", ModeAuto, call("mcp__danger", `{}`), tools.ActionDeny},
		{"other mcp prompts", ModeAllowRead, call("mcp__safe", `{}`), tools.ActionAsk},
		// a denied core writer refuses even under allow-all.
		{"deny write tool", ModeAllowAll, call("write", `{}`), tools.ActionDeny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b.SetMode(c.mode)
			d := b.Guard()(context.Background(), c.call)
			assert.Equal(t, c.want, d.Action)
		})
	}

	// user-initiated ! lines are not exempt from a configured denial.
	ctx := tools.WithUserInitiated(context.Background())
	d := b.Guard()(ctx, bashCall("git stash"))
	assert.Equal(t, tools.ActionDeny, d.Action)

	// clearing the list lifts the denial for a previously listed command.
	b.SetMode(ModeAllowRead)
	b.SetDeniedCommands(nil)
	d = b.Guard()(context.Background(), bashCall("git stash"))
	assert.NotEqual(t, tools.ActionDeny, d.Action)
}

func TestGuardUserInitiatedExemptInEveryMode(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	for _, m := range []Mode{ModeAllowAll, ModeAllowRead, ModeAuto, ModeBlockAll} {
		b.SetMode(m)
		ctx := tools.WithUserInitiated(context.Background())
		d := b.Guard()(ctx, call("write", `{}`))
		assert.Equal(t, tools.ActionAllow, d.Action, m.String())
	}
}

func TestGuardDoomedEditRunsInsteadOfPrompting(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	failDryRun := func(agent.ToolCall) error { return errors.New("missing") }
	b.SetDryRun(failDryRun)
	d := b.Guard()(context.Background(), call("edit", `{"path":"x.go","edits":[]}`))
	assert.Equal(t, tools.ActionAllow, d.Action)

	// without a dry run the barrier cannot predict and prompts.
	b2 := newTestBarrier(newFakePrompter())
	d = b2.Guard()(context.Background(), call("edit", `{"path":"x.go","edits":[]}`))
	assert.Equal(t, tools.ActionAsk, d.Action)
}

func TestAskerNoUIdeniesWhenHeadless(t *testing.T) {
	t.Parallel()

	b := NewBarrier(noRO) // no prompter installed
	d := runAsk(b, context.Background(), "write", []byte(`{}`))
	assert.Equal(t, tools.ActionDeny, d.Action)
	assert.Contains(t, d.Reason, "permission required")
}

func TestAskerNoUIdeniesReadsUnderBlockAll(t *testing.T) {
	t.Parallel()

	b := NewBarrier(noRO)
	b.SetMode(ModeBlockAll)
	d := runAsk(b, context.Background(), "bash", []byte(`{"command":"ls -la"}`))
	assert.Equal(t, tools.ActionDeny, d.Action)
}

func TestAskerOpenErrNoUIdenies(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	p.err = errors.New("tui: no interactive terminal")
	b := newTestBarrier(p)
	d := runAsk(b, context.Background(), "write", []byte(`{}`))
	assert.Equal(t, tools.ActionDeny, d.Action)
}

func TestAskerAllowThisCallOnly(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	// first call prompts and is allowed.
	d1 := runAndAnswer(t, p, b, "write", []byte(`{}`), int(optAllow))
	assert.Equal(t, tools.ActionAllow, d1.Action)

	// a second identical write still prompts (no session memory for this-call-only).
	assert.Equal(t, tools.ActionAsk, b.Guard()(context.Background(), call("write", `{}`)).Action)
}

func TestAskerDenyCapturesReason(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	p.reasons["reason for denying"] = "not now"
	b := newTestBarrier(p)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "write", []byte(`{}`)); close(done) }()
	waitDialog(t, p).answer(int(optDeny))
	<-done

	assert.Equal(t, tools.ActionDeny, got.Action)
	assert.Contains(t, got.Reason, "not now")
}

func TestAskerAllowWithNoteInjectsSteering(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	p.reasons["note for allowing"] = "only inside build/"
	n := &fakeNoter{}
	b := newTestBarrier(p)
	b.SetNoter(n.Note)

	got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"rm build"}`), int(optAllowNote))

	assert.Equal(t, tools.ActionAllow, got.Action)
	notes := n.all()
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "User allowed `rm`")
	assert.Contains(t, notes[0], "only inside build/")
}

func TestAskerAllowForSessionShortCircuitsNextCall(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"git status"}`), int(optAllowSession))
	assert.Equal(t, tools.ActionAllow, got.Action)

	// a different git command matches the same bash:git grant and opens no dialog.
	n := p.count()
	d2 := runAsk(b, context.Background(), "bash", []byte(`{"command":"git log --oneline"}`))
	assert.Equal(t, tools.ActionAllow, d2.Action)
	assert.Equal(t, n, p.count())
}

func TestAskerSessionGrantIsToolScopedNotGlobal(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	got := runAndAnswer(t, p, b, "write", []byte(`{}`), int(optAllowSession))
	assert.Equal(t, tools.ActionAllow, got.Action)

	// a different tool name is not covered by the write grant.
	assert.Equal(t, tools.ActionAsk, b.Guard()(context.Background(), call("edit", `{}`)).Action)
}

func TestAskerCompoundGrantCoversOnlyCompoundCommands(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	// a piped command takes the broad grant; derive its display index from the actions.
	displayIdx := slices.Index(optionActions("cat a | sort"), int(optAllowCompound))
	got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"cat a | sort"}`), displayIdx)
	assert.Equal(t, tools.ActionAllow, got.Action)

	// another compound command is covered without a new dialog.
	_, ok := b.sessionAllowed(bashCall("grep foo f && wc -l"))
	assert.True(t, ok)

	// a plain (non-compound) command does not match the broad grant.
	_, ok = b.sessionAllowed(bashCall("rm build"))
	assert.False(t, ok)
}

func TestAskerCompoundCommandNeverMatchesNamedGrant(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	// grant git for session.
	got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"git status"}`), int(optAllowSession))
	assert.Equal(t, tools.ActionAllow, got.Action)

	// a piped git command is compound and never matches the named git grant.
	_, ok := b.sessionAllowed(bashCall("git log | head"))
	assert.False(t, ok)
}

func TestAskerAutoClassifiesReadOnlyAndResolvesDialog(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)
	b.SetMode(ModeAuto)
	cl := &fakeClassifier{verdict: ClassReadOnly}
	b.SetClassifier(cl)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`)); close(done) }()
	<-done // the classifier resolves allow without a keystroke

	assert.Equal(t, tools.ActionAllow, got.Action)
}

func TestAskerAutoWriteVerdictKeepsDialogWaiting(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)
	b.SetMode(ModeAuto)
	cl := &fakeClassifier{verdict: ClassWrite}
	b.SetClassifier(cl)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`)); close(done) }()

	// the write verdict must not resolve; a user keystroke still decides.
	waitDialog(t, p).answer(int(optDeny))
	<-done

	assert.Equal(t, tools.ActionDeny, got.Action)
}

func TestAskerAutoClassifiesClearWriteAndApprovesOnReadOnly(t *testing.T) {
	t.Parallel()

	b := NewBarrier(noRO)
	b.SetMode(ModeAuto)
	p := newFakePrompter()
	b.SetPrompter(p)
	cl := &fakeClassifier{verdict: ClassReadOnly}
	b.SetClassifier(cl)

	// a confident write (rm) is still classified in auto mode; a readonly verdict
	// resolves the dialog open without a keystroke.
	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"rm build"}`)); close(done) }()
	<-done

	assert.Equal(t, 1, cl.calls)
	assert.Equal(t, tools.ActionAllow, got.Action)
}

func TestAskerAutoClassifiesNativeWriteTool(t *testing.T) {
	t.Parallel()

	b := NewBarrier(noRO)
	b.SetMode(ModeAuto)
	p := newFakePrompter()
	b.SetPrompter(p)
	cl := &fakeClassifier{verdict: ClassReadOnly}
	b.SetClassifier(cl)

	// native write/edit tools are classified too, so the demo server can approve them.
	var got tools.Decision
	done := make(chan struct{})
	go func() {
		got = runAsk(b, t.Context(), "write", []byte(`{"path":"a.txt","content":"x"}`))
		close(done)
	}()
	<-done

	assert.Equal(t, 1, cl.calls)
	assert.Equal(t, tools.ActionAllow, got.Action)
}

func TestAskerAutoReadOnlyEmitsNotice(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)
	b.SetMode(ModeAuto)
	b.SetClassifier(&fakeClassifier{verdict: ClassReadOnly})
	n := &noticeRecorder{}
	b.SetNotice(n.record)

	done := make(chan struct{})
	go func() {
		runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`))
		close(done)
	}()
	<-done // the classifier resolves allow; no keystroke needed

	require.Eventually(t, func() bool { return len(n.all()) == 1 }, time.Second, 10*time.Millisecond)
	assert.Contains(t, n.all()[0], "auto-allowed as read-only")
}

func TestAskerAutoWriteVerdictEmitsNoNotice(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)
	b.SetMode(ModeAuto)
	b.SetClassifier(&fakeClassifier{verdict: ClassWrite})
	n := &noticeRecorder{}
	b.SetNotice(n.record)

	go runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`))
	d := waitDialog(t, p)
	d.answer(int(optDeny)) // the user decides; no auto-allow notice fires
	require.Empty(t, n.all())
}

// noticeRecorder captures notices under a lock for race-free assertions.
type noticeRecorder struct {
	mu      sync.Mutex
	notices []string
}

func (r *noticeRecorder) record(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, s)
}
func (r *noticeRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.notices...)
}

func TestAskerAutoUserAnswerCancelsInFlightClassification(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)
	b.SetMode(ModeAuto)
	cancelled := make(chan struct{})
	cl := &blockingClassifier{cancel: cancelled}
	b.SetClassifier(cl)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`)); close(done) }()

	waitDialog(t, p).answer(int(optDeny)) // the user answers before classification returns
	<-done

	assert.Equal(t, tools.ActionDeny, got.Action)
	require.Eventually(t, func() bool {
		select {
		case <-cancelled:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

// blockingClassifier waits for ctx cancellation and reports it.
type blockingClassifier struct{ cancel chan struct{} }

func (c *blockingClassifier) Classify(ctx context.Context, command string) Class {
	<-ctx.Done()
	close(c.cancel)
	return ClassUnsure
}

func TestSetModeResolvesOpenDialogAsAllowForAllowAll(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "write", []byte(`{}`)); close(done) }()
	_ = waitDialog(t, p)

	b.SetMode(ModeAllowAll) // resolving the open dialog as allow
	<-done

	assert.Equal(t, tools.ActionAllow, got.Action)
}

func TestSetModeOutOfBlockAllResolvesReadOnlyDialogAsAllow(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)
	b.SetMode(ModeBlockAll)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"ls -la"}`)); close(done) }()
	_ = waitDialog(t, p)

	b.SetMode(ModeAllowRead) // a read becomes auto-allowed
	<-done

	assert.Equal(t, tools.ActionAllow, got.Action)
}

func TestSetModeIntoBlockAllLeavesWriteDialogWaiting(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	var got tools.Decision
	done := make(chan struct{})
	go func() { got = runAsk(b, t.Context(), "write", []byte(`{}`)); close(done) }()
	_ = waitDialog(t, p)

	b.SetMode(ModeBlockAll) // block-all still prompts writes; dialog stays open

	waitDialog(t, p).answer(int(optDeny))
	<-done
	assert.Equal(t, tools.ActionDeny, got.Action)
}

func TestCycleAdvancesModesInOrder(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	want := []Mode{ModeAuto, ModeBlockAll, ModeAllowAll, ModeAllowRead}
	for _, w := range want {
		assert.Equal(t, w, b.Cycle())
	}
}

func TestRejectionReasonNamesTheRefusal(t *testing.T) {
	t.Parallel()

	r := rejectionReason(bashCall(`sed -i s/a/b/ f`))
	assert.Contains(t, r, "edit tool")
	assert.NotContains(t, strings.ToLower(r), "allow")
}
