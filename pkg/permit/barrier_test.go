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

func (c *fakeClassifier) Classify(ctx context.Context, s Subject) Class {
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
		{"allow_all_readonly", ModeAllowAll, bashCall("ls -la"), tools.ActionAllow},
		{"allow_all_write", ModeAllowAll, call("write", `{}`), tools.ActionAllow},
		{"allow_all_reject", ModeAllowAll, bashCall(`sed -i s/a/b/ f`), tools.ActionAllow},

		// allow-read runs reads, asks writes and unverifiable, denies sed -i.
		{"read_readonly", ModeAllowRead, bashCall("ls -la"), tools.ActionAllow},
		{"read_write", ModeAllowRead, call("write", `{}`), tools.ActionAsk},
		{"read_reject", ModeAllowRead, bashCall(`sed -i s/a/b/ f`), tools.ActionDeny},
		{"read_unverifiable", ModeAllowRead, bashCall("stat f"), tools.ActionAsk},

		// auto+write keeps every other mode's bar; write scope is covered separately.
		{"autowrite_readonly", ModeAutoWrite, bashCall("ls -la"), tools.ActionAllow},
		{"autowrite_unscoped_write", ModeAutoWrite, call("write", `{"path":"/etc/hosts"}`), tools.ActionAsk},
		{"autowrite_reject", ModeAutoWrite, bashCall(`sed -i s/a/b/ f`), tools.ActionDeny},
		{"autowrite_unverifiable", ModeAutoWrite, bashCall("stat f"), tools.ActionAsk},

		// block-all asks reads and writes alike; sed stays a hard deny.
		{"block_readonly", ModeBlockAll, bashCall("ls -la"), tools.ActionAsk},
		{"block_write", ModeBlockAll, call("write", `{}`), tools.ActionAsk},
		{"block_reject", ModeBlockAll, bashCall(`sed -i s/a/b/ f`), tools.ActionDeny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBarrier(newFakePrompter())
			b.SetMode(c.mode)
			d := b.Guard()(t.Context(), c.call)
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
		{"safe_bash_read", ModeAllowRead, bashCall("git status"), tools.ActionAllow},
		{"safe_bash_auto", ModeAuto, bashCall("git status"), tools.ActionAllow},
		// whitespace is tolerated; a different command still prompts.
		{"safe_bash_padded", ModeAllowRead, bashCall("  git status "), tools.ActionAllow},
		{"unsafe_bash_prompt", ModeAllowRead, bashCall("git push"), tools.ActionAsk},
		// an exact MCP tool name auto-runs without needing registry metadata.
		{"safe_mcp_tool", ModeAllowRead, call("mcp__list", `{}`), tools.ActionAllow},
		{"other_mcp_prompt", ModeAllowRead, call("mcp__write", `{}`), tools.ActionAsk},
		// block-all still asks even a listed safe command.
		{"safe_bash_block", ModeBlockAll, bashCall("git status"), tools.ActionAsk},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b.SetMode(c.mode)
			d := b.Guard()(t.Context(), c.call)
			assert.Equal(t, c.want, d.Action)
		})
	}

	// a config entry can never un-reject an in-place write.
	b2 := newTestBarrier(newFakePrompter())
	b2.SetSafeCommands([]string{"sed -i s/a/b/ f"})
	d := b2.Guard()(t.Context(), bashCall("sed -i s/a/b/ f"))
	assert.Equal(t, tools.ActionDeny, d.Action)

	// write/edit are excluded from safe matching by name.
	b3 := newTestBarrier(newFakePrompter())
	b3.SetSafeCommands([]string{"write", "edit"})
	assert.Equal(t, tools.ActionAsk, b3.Guard()(t.Context(), call("write", `{}`)).Action)
	assert.Equal(t, tools.ActionAsk, b3.Guard()(t.Context(), call("edit", `{"path":"x.go","edits":[]}`)).Action)
}

func TestGuardSafeCommandsMatchMCPServerNamespace(t *testing.T) {
	t.Parallel()

	// naming an MCP server (tools register as server__tool) covers every tool it exposes.
	b := newTestBarrier(newFakePrompter())
	b.SetSafeCommands([]string{"sectool"})

	for _, name := range []string{"sectool__proxy_poll", "sectool__flow_get", "sectool__js_surface"} {
		d := b.Guard()(t.Context(), call(name, `{}`))
		assert.Equal(t, tools.ActionAllow, d.Action, name)
	}

	// a different server's tool still prompts.
	b2 := newTestBarrier(newFakePrompter())
	b2.SetSafeCommands([]string{"sectool"})
	d := b2.Guard()(t.Context(), call("github__get_repo", `{}`))
	assert.Equal(t, tools.ActionAsk, d.Action)

	// an exact namespaced tool name still matches that one tool only.
	b3 := newTestBarrier(newFakePrompter())
	b3.SetSafeCommands([]string{"sectool__proxy_poll"})
	assert.Equal(t, tools.ActionAllow, b3.Guard()(t.Context(), call("sectool__proxy_poll", `{}`)).Action)
	assert.Equal(t, tools.ActionAsk, b3.Guard()(t.Context(), call("sectool__flow_get", `{}`)).Action)
}

func TestGuardSafeMatchesBashCommandComponents(t *testing.T) {
	t.Parallel()

	// a compound line matches when every component is either the listed entry or
	// verifiably read-only; wrapping in cd/pipe no longer defeats the safe match.
	b := newTestBarrier(newFakePrompter())
	b.SetSafeCommands([]string{"make lint"})
	d := b.Guard()(t.Context(), bashCall("cd /tmp && make lint 2>&1 | tail -5"))
	assert.Equal(t, tools.ActionAllow, d.Action)

	// an appended write never rides in on a listed prefix.
	b2 := newTestBarrier(newFakePrompter())
	b2.SetSafeCommands([]string{"make lint"})
	d = b2.Guard()(t.Context(), bashCall("make lint && rm -rf /tmp/x"))
	assert.Equal(t, tools.ActionAsk, d.Action)

	// a compound whose unlisted component is neither read-only nor listed prompts.
	b3 := newTestBarrier(newFakePrompter())
	b3.SetSafeCommands([]string{"make lint"})
	d = b3.Guard()(t.Context(), bashCall("cd /tmp && make test"))
	assert.Equal(t, tools.ActionAsk, d.Action)

	// unsafe ops (redirect/substitution) defeat component matching entirely.
	b4 := newTestBarrier(newFakePrompter())
	b4.SetSafeCommands([]string{"make lint", "tail"})
	d = b4.Guard()(t.Context(), bashCall("cd /tmp && make lint > out.txt"))
	assert.Equal(t, tools.ActionAsk, d.Action)
}

func TestGuardDenyMatchesBashCommandComponents(t *testing.T) {
	t.Parallel()

	// a denied command nested in a compound is still refused.
	b := newTestBarrier(newFakePrompter())
	b.SetMode(ModeAllowAll)
	b.SetDeniedCommands([]string{"git stash"})
	d := b.Guard()(t.Context(), bashCall("cd /x && git stash push -m wip"))
	assert.Equal(t, tools.ActionDeny, d.Action)
}

func TestGuardDeniedCommandsMatchMCPServerNamespace(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	b.SetDeniedCommands([]string{"sectool"})
	assert.Equal(t, tools.ActionDeny, b.Guard()(t.Context(), call("sectool__proxy_poll", `{}`)).Action)
	// naming one server does not deny another.
	assert.Equal(t, tools.ActionAsk, b.Guard()(t.Context(), call("github__get_repo", `{}`)).Action)
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
		{"deny_git_stash_allow_all", ModeAllowAll, bashCall("git stash"), tools.ActionDeny},
		{"deny_git_stash_subcommand", ModeAuto, bashCall("git stash push -m x"), tools.ActionDeny},
		// whitespace is tolerated; a different command still asks.
		{"deny_padded", ModeAllowRead, bashCall("  git stash "), tools.ActionDeny},
		{"unlisted_bash_prompts", ModeAllowRead, bashCall("git push"), tools.ActionAsk},
		// an exact tool name denies regardless of registry metadata.
		{"deny_mcp_tool", ModeAuto, call("mcp__danger", `{}`), tools.ActionDeny},
		{"other_mcp_prompts", ModeAllowRead, call("mcp__safe", `{}`), tools.ActionAsk},
		// a denied core writer refuses even under allow-all.
		{"deny_write_tool", ModeAllowAll, call("write", `{}`), tools.ActionDeny},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b.SetMode(c.mode)
			d := b.Guard()(t.Context(), c.call)
			assert.Equal(t, c.want, d.Action)
		})
	}

	// a user's own ! line runs regardless of config denial; the human owns that shell.
	ctx := tools.WithUserInitiated(t.Context())
	d := b.Guard()(ctx, bashCall("git stash"))
	assert.Equal(t, tools.ActionAllow, d.Action)

	// clearing the list lifts the denial for a previously listed command.
	b.SetMode(ModeAllowRead)
	b.SetDeniedCommands(nil)
	d = b.Guard()(t.Context(), bashCall("git stash"))
	assert.NotEqual(t, tools.ActionDeny, d.Action)
}

func TestGuardUserInitiatedExemptInEveryMode(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	for _, m := range []Mode{ModeAllowAll, ModeAllowRead, ModeAuto, ModeBlockAll} {
		b.SetMode(m)
		ctx := tools.WithUserInitiated(t.Context())
		d := b.Guard()(ctx, call("write", `{}`))
		assert.Equal(t, tools.ActionAllow, d.Action, m.String())
	}
}

func TestGuardDoomedEditRunsInsteadOfPrompting(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	failDryRun := func(agent.ToolCall) error { return errors.New("missing") }
	b.SetDryRun(failDryRun)
	d := b.Guard()(t.Context(), call("edit", `{"path":"x.go","edits":[]}`))
	assert.Equal(t, tools.ActionAllow, d.Action)

	// without a dry run the barrier cannot predict and prompts.
	b2 := newTestBarrier(newFakePrompter())
	d = b2.Guard()(t.Context(), call("edit", `{"path":"x.go","edits":[]}`))
	assert.Equal(t, tools.ActionAsk, d.Action)
}

func TestAskerNoUI(t *testing.T) {
	t.Parallel()

	// no prompter installed denies headless.
	t.Run("denies_when_headless", func(t *testing.T) {
		b := NewBarrier(noRO) // no prompter installed
		d := runAsk(b, t.Context(), "write", []byte(`{}`))
		assert.Equal(t, tools.ActionDeny, d.Action)
		assert.Contains(t, d.Reason, "permission required")
	})

	t.Run("denies_reads_under_block_all", func(t *testing.T) {
		b := NewBarrier(noRO)
		b.SetMode(ModeBlockAll)
		d := runAsk(b, t.Context(), "bash", []byte(`{"command":"ls -la"}`))
		assert.Equal(t, tools.ActionDeny, d.Action)
	})

	t.Run("open_err_no_ui_denies", func(t *testing.T) {
		p := newFakePrompter()
		p.err = errors.New("tui: no interactive terminal")
		b := newTestBarrier(p)
		d := runAsk(b, t.Context(), "write", []byte(`{}`))
		assert.Equal(t, tools.ActionDeny, d.Action)
	})
}

func TestAskerAllowThisCallOnly(t *testing.T) {
	t.Parallel()

	p := newFakePrompter()
	b := newTestBarrier(p)

	// first call prompts and is allowed.
	d1 := runAndAnswer(t, p, b, "write", []byte(`{}`), int(optAllow))
	assert.Equal(t, tools.ActionAllow, d1.Action)

	// a second identical write still prompts (no session memory for this-call-only).
	assert.Equal(t, tools.ActionAsk, b.Guard()(t.Context(), call("write", `{}`)).Action)
}

func TestAskerAllowEmitsDescriptiveNotices(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input []byte
		opt   int
		want  string
	}{
		{"this_time", []byte(`{"command":"git status"}`), optAllow, "Tool call allowed this time"},
		{"session", []byte(`{"command":"ls -la"}`), optAllowSession, "Tool allowed for session"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newFakePrompter() // a fresh prompter per case so waitDialog targets its own dialog
			b := newTestBarrier(p)
			n := &noticeRecorder{}
			b.SetNotice(n.record)

			got := runAndAnswer(t, p, b, "bash", c.input, c.opt)
			assert.Equal(t, tools.ActionAllow, got.Action)
			require.Len(t, n.all(), 1)
			assert.Equal(t, c.want, n.all()[0])
		})
	}
}

func TestAskerDenyAndNotes(t *testing.T) {
	t.Parallel()

	// a deny captures the canned reason.
	t.Run("deny_captures_reason", func(t *testing.T) {
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
	})

	// an allow-with-note injects steering into the session.
	t.Run("allow_with_note_injects_steering", func(t *testing.T) {
		p := newFakePrompter()
		p.reasons["note for allowing"] = "only inside build/"
		n := &fakeNoter{}
		b := newTestBarrier(p)
		b.SetNoter(n.Note)

		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"rm build"}`), int(optAllowNote))

		assert.Equal(t, tools.ActionAllow, got.Action)
		notes := n.all()
		require.Len(t, notes, 1)
		assert.Contains(t, notes[0], "Allowed with note:")
		assert.Contains(t, notes[0], "only inside build/")
	})

	// a deny injects its reason as a note.
	t.Run("deny_injects_note_when_reason_given", func(t *testing.T) {
		p := newFakePrompter()
		p.reasons["reason for denying"] = "not now"
		n := &fakeNoter{}
		b := newTestBarrier(p)
		b.SetNoter(n.Note)

		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"rm build"}`), int(optDeny))

		assert.Equal(t, tools.ActionDeny, got.Action)
		notes := n.all()
		require.Len(t, notes, 1)
		assert.Contains(t, notes[0], "Denied with note:")
		assert.Contains(t, notes[0], "not now")
	})

	// a deny without a canned reason injects nothing.
	t.Run("deny_no_reason_injects_nothing", func(t *testing.T) {
		p := newFakePrompter() // no canned reason -> Reason returns false
		n := &fakeNoter{}
		b := newTestBarrier(p)
		b.SetNoter(n.Note)

		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"rm build"}`), int(optDeny))

		assert.Equal(t, tools.ActionDeny, got.Action)
		require.Empty(t, n.all())
	})
}

func TestAskerSessionGrants(t *testing.T) {
	t.Parallel()

	// an allow-for-session bash:git grant covers a different git command without a new dialog.
	t.Run("allow_for_session_short_circuits_next_call", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"git status"}`), int(optAllowSession))
		assert.Equal(t, tools.ActionAllow, got.Action)

		// a different git command matches the same bash:git grant and opens no dialog.
		n := p.count()
		d2 := runAsk(b, t.Context(), "bash", []byte(`{"command":"git log --oneline"}`))
		assert.Equal(t, tools.ActionAllow, d2.Action)
		assert.Equal(t, n, p.count())
	})

	// a write grant is tool-scoped and does not cover edit.
	t.Run("session_grant_is_tool_scoped_not_global", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		got := runAndAnswer(t, p, b, "write", []byte(`{}`), int(optAllowSession))
		assert.Equal(t, tools.ActionAllow, got.Action)

		// a different tool name is not covered by the write grant.
		assert.Equal(t, tools.ActionAsk, b.Guard()(t.Context(), call("edit", `{}`)).Action)
	})

	// a compound command takes the broad grant and covers only other compounds.
	t.Run("compound_grant_covers_only_compound_commands", func(t *testing.T) {
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
	})

	// a piped git never matches the named bash:git grant.
	t.Run("compound_command_never_matches_named_grant", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		// grant git for session.
		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"git status"}`), int(optAllowSession))
		assert.Equal(t, tools.ActionAllow, got.Action)

		// a piped git command is compound and never matches the named git grant.
		_, ok := b.sessionAllowed(bashCall("git log | head"))
		assert.False(t, ok)
	})

	// one non-readonly head grants per-name memory for it.
	t.Run("compound_single_head_grants_per_name_for_session", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		// ifconfig is the one non-readonly head; read-only `head` doesn't count, so the
		// dialog offers per-name memory and answering it remembers bash:ifconfig.
		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"ifconfig | head -n 10"}`), int(optAllowSession))
		assert.Equal(t, tools.ActionAllow, got.Action)

		// the named grant covers a plain ifconfig call (no new dialog).
		n := p.count()
		d1 := runAsk(b, t.Context(), "bash", []byte(`{"command":"ifconfig eth0"}`))
		assert.Equal(t, tools.ActionAllow, d1.Action)
		assert.Equal(t, n, p.count())

		// and a future compound whose only non-readonly head is ifconfig.
		d2 := runAsk(b, t.Context(), "bash", []byte(`{"command":"ifconfig | grep flags"}`))
		assert.Equal(t, tools.ActionAllow, d2.Action)

		// an unrelated write command is not covered by the named grant.
		_, ok := b.sessionAllowed(bashCall("rm build"))
		assert.False(t, ok)
	})

	// three distinct non-readonly heads defeat per-name memory and fall back to a broad grant.
	t.Run("compound_multiple_heads_falls_back_to_broad_grant", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		// three distinct non-readonly heads (rm, mkdir, touch) defeat per-name memory; the
		// dialog offers the broad grant instead.
		displayIdx := slices.Index(optionActions("rm a && mkdir b && touch c"), int(optAllowCompound))
		require.NotEqual(t, -1, displayIdx)
		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"rm a && mkdir b && touch c"}`), displayIdx)
		assert.Equal(t, tools.ActionAllow, got.Action)

		// the broad grant covers a different compound command.
		_, ok := b.sessionAllowed(bashCall("touch x | wc -l"))
		assert.True(t, ok)

		// but not a plain (non-compound) command.
		_, ok = b.sessionAllowed(bashCall("rm build"))
		assert.False(t, ok)
	})

	// two non-readonly heads are both granted for the session by name.
	t.Run("compound_two_heads_grants_both_for_session", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		// rm and mkdir are the two non-readonly heads; answering allow-for-session adds both.
		got := runAndAnswer(t, p, b, "bash", []byte(`{"command":"rm build && mkdir dir"}`), int(optAllowSession))
		assert.Equal(t, tools.ActionAllow, got.Action)

		// each command is granted on its own (no new dialog).
		n := p.count()
		for _, cmd := range []string{"rm old", "mkdir fresh"} {
			d := runAsk(b, t.Context(), "bash", []byte(`{"command":`+strconvQuote(cmd)+`}`))
			assert.Equal(t, tools.ActionAllow, d.Action)
		}
		assert.Equal(t, n+0, p.count())

		// a compound whose non-readonly heads are both granted matches by name.
		d2 := runAsk(b, t.Context(), "bash", []byte(`{"command":"rm build && mkdir dir"}`))
		assert.Equal(t, tools.ActionAllow, d2.Action)

		// an unrelated write is not covered.
		_, ok := b.sessionAllowed(bashCall("touch x"))
		assert.False(t, ok)
	})
}

func TestAskerAuto(t *testing.T) {
	t.Parallel()

	// a read-only verdict resolves the open dialog without a keystroke.
	t.Run("classifies_read_only_and_resolves_dialog", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetMode(ModeAuto)
		cl := &fakeClassifier{verdict: ClassAllow}
		b.SetClassifier(cl)

		var got tools.Decision
		done := make(chan struct{})
		go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`)); close(done) }()
		<-done // the classifier resolves allow without a keystroke

		assert.Equal(t, tools.ActionAllow, got.Action)
	})

	// a write verdict must not resolve; a user keystroke still decides.
	t.Run("write_verdict_keeps_dialog_waiting", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetMode(ModeAuto)
		cl := &fakeClassifier{verdict: ClassDeny}
		b.SetClassifier(cl)

		var got tools.Decision
		done := make(chan struct{})
		go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`)); close(done) }()

		// the write verdict must not resolve; a user keystroke still decides.
		waitDialog(t, p).answer(int(optDeny))
		<-done

		assert.Equal(t, tools.ActionDeny, got.Action)
	})

	// a confident write (rm) is still classified in auto mode; a readonly verdict resolves the dialog open.
	t.Run("classifies_clear_write_and_approves_on_read_only", func(t *testing.T) {
		b := NewBarrier(noRO)
		b.SetMode(ModeAuto)
		p := newFakePrompter()
		b.SetPrompter(p)
		cl := &fakeClassifier{verdict: ClassAllow}
		b.SetClassifier(cl)

		// a confident write (rm) is still classified in auto mode; a readonly verdict
		// resolves the dialog open without a keystroke.
		var got tools.Decision
		done := make(chan struct{})
		go func() { got = runAsk(b, t.Context(), "bash", []byte(`{"command":"rm build"}`)); close(done) }()
		<-done

		assert.Equal(t, 1, cl.calls)
		assert.Equal(t, tools.ActionAllow, got.Action)
	})

	// an auto-approved readonly call emits a notice.
	t.Run("read_only_emits_notice", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetMode(ModeAuto)
		b.SetClassifier(&fakeClassifier{verdict: ClassAllow})
		n := &noticeRecorder{}
		b.SetNotice(n.record)

		done := make(chan struct{})
		go func() {
			runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`))
			close(done)
		}()
		<-done // the classifier resolves allow; no keystroke needed

		require.Eventually(t, func() bool { return len(n.all()) == 1 }, time.Second, 10*time.Millisecond)
		assert.Equal(t, "Tool auto allowed", n.all()[0])
	})

	// a user-decided write verdict emits no auto-allow notice.
	t.Run("write_verdict_emits_no_notice", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetMode(ModeAuto)
		b.SetClassifier(&fakeClassifier{verdict: ClassDeny})
		n := &noticeRecorder{}
		b.SetNotice(n.record)

		go runAsk(b, t.Context(), "bash", []byte(`{"command":"stat f.txt"}`))
		d := waitDialog(t, p)
		d.answer(int(optDeny)) // the user decides; no auto-allow notice fires
		require.Empty(t, n.all())
	})

	// answering before classification returns cancels the in-flight classifier.
	t.Run("user_answer_cancels_in_flight_classification", func(t *testing.T) {
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
	})

	// plain auto judges shell commands only; MCP calls are never classified.
	t.Run("does_not_classify_non_bash_calls", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetMode(ModeAuto) // plain auto judges shell commands only
		cl := &fakeClassifier{verdict: ClassAllow}
		b.SetClassifier(cl)

		// an MCP call is never sent to the classifier in auto mode; it just waits on the dialog.
		done := make(chan struct{})
		go func() { runAsk(b, t.Context(), "mcp__list", []byte(`{}`)); close(done) }()
		waitDialog(t, p).answer(int(optAllow))
		<-done

		assert.Equal(t, 0, cl.calls)
	})

	// auto+mcp also classifies MCP/extension calls.
	t.Run("mcp_classifies_non_bash_calls", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetMode(ModeAutoMCP) // auto+mcp also classifies MCP/extension calls
		cl := &fakeClassifier{verdict: ClassAllow}
		b.SetClassifier(cl)

		var got tools.Decision
		done := make(chan struct{})
		go func() { got = runAsk(b, t.Context(), "mcp__list", []byte(`{}`)); close(done) }()
		<-done // the readonly verdict resolves without a keystroke

		assert.Equal(t, tools.ActionAllow, got.Action)
		assert.Equal(t, 1, cl.calls)
	})
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

// blockingClassifier waits for ctx cancellation and reports it.
type blockingClassifier struct{ cancel chan struct{} }

func (c *blockingClassifier) Classify(ctx context.Context, s Subject) Class {
	<-ctx.Done()
	close(c.cancel)
	return ClassUnsure
}

func TestSetModeResolvesOpenDialog(t *testing.T) {
	t.Parallel()

	// switching to allow-all resolves the open dialog as an allow.
	t.Run("resolves_open_dialog_as_allow_for_allow_all", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)

		var got tools.Decision
		done := make(chan struct{})
		go func() { got = runAsk(b, t.Context(), "write", []byte(`{}`)); close(done) }()
		_ = waitDialog(t, p)

		b.SetMode(ModeAllowAll) // resolving the open dialog as allow
		<-done

		assert.Equal(t, tools.ActionAllow, got.Action)
	})

	// leaving block-all auto-allows a pending read.
	t.Run("out_of_block_all_resolves_read_only_dialog_as_allow", func(t *testing.T) {
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
	})

	// block-all still prompts writes; the dialog stays open.
	t.Run("into_block_all_leaves_write_dialog_waiting", func(t *testing.T) {
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
	})
}

func TestGuardAutoWriteScope(t *testing.T) {
	t.Parallel()

	cwd := t.TempDir()
	outside := t.TempDir()

	newScoped := func() *Barrier {
		b := newTestBarrier(newFakePrompter())
		b.SetWriteRoots(cwd)
		b.SetMode(ModeAutoWrite)
		return b
	}

	cases := []struct {
		name string
		call agent.ToolCall
		want tools.Action
	}{
		{"in_scope_write", call("write", `{"path":"`+cwd+`/a.go"}`), tools.ActionAllow},
		{"in_scope_edit", call("edit", `{"path":"`+cwd+`/a.go"}`), tools.ActionAllow},
		{"out_of_scope_write", call("write", `{"path":"`+outside+`/a.go"}`), tools.ActionAsk},
		{"unparsable_write", call("write", `{}`), tools.ActionAsk},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newScoped().Guard()(t.Context(), c.call)
			assert.Equal(t, c.want, d.Action)
		})
	}

	t.Run("mkdir_in_scope_allows", func(t *testing.T) {
		d := newScoped().Guard()(t.Context(), bashCall("mkdir -p build/out"))
		assert.Equal(t, tools.ActionAllow, d.Action)
	})

	t.Run("mkdir_outside_asks", func(t *testing.T) {
		d := newScoped().Guard()(t.Context(), bashCall("mkdir /etc/x"))
		assert.Equal(t, tools.ActionAsk, d.Action)
	})

	t.Run("mkdir_asks_in_other_modes", func(t *testing.T) {
		b := newTestBarrier(newFakePrompter())
		b.SetWriteRoots(cwd)
		b.SetMode(ModeAutoMCP)
		d := b.Guard()(t.Context(), bashCall("mkdir build"))
		assert.Equal(t, tools.ActionAsk, d.Action)
	})

	t.Run("roots_unset_asks", func(t *testing.T) {
		b := newTestBarrier(newFakePrompter())
		b.SetMode(ModeAutoWrite)
		d := b.Guard()(t.Context(), call("write", `{"path":"`+cwd+`/a.go"}`))
		assert.Equal(t, tools.ActionAsk, d.Action)
	})

	t.Run("denied_command_outranks", func(t *testing.T) {
		b := newScoped()
		b.SetDeniedCommands([]string{"write"})
		d := b.Guard()(t.Context(), call("write", `{"path":"`+cwd+`/a.go"}`))
		assert.Equal(t, tools.ActionDeny, d.Action)
	})

	t.Run("other_modes_still_ask", func(t *testing.T) {
		for _, m := range []Mode{ModeAllowRead, ModeAuto, ModeAutoMCP, ModeBlockAll} {
			b := newTestBarrier(newFakePrompter())
			b.SetWriteRoots(cwd)
			b.SetMode(m)
			d := b.Guard()(t.Context(), call("write", `{"path":"`+cwd+`/a.go"}`))
			assert.Equal(t, tools.ActionAsk, d.Action, m)
		}
	})
}

// TestAskerNeverClassifiesBuiltins pins that a core writer reaching the dialog is
// never handed to the model: a readonly verdict would otherwise auto-allow it.
func TestAskerNeverClassifiesBuiltins(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		mode  Mode
		tool  string
		input string
	}{
		{"autowrite_out_of_scope_write", ModeAutoWrite, "write", `{"path":"/etc/hosts"}`},
		{"autowrite_out_of_scope_edit", ModeAutoWrite, "edit", `{"path":"/etc/hosts"}`},
		{"autompc_write", ModeAutoMCP, "write", `{"path":"/etc/hosts"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newFakePrompter()
			b := newTestBarrier(p)
			cl := &fakeClassifier{verdict: ClassAllow} // would auto-allow if ever consulted
			b.SetClassifier(cl)
			b.SetWriteRoots(t.TempDir())
			b.SetMode(c.mode)

			got := runAndAnswer(t, p, b, c.tool, []byte(c.input), int(optDeny))
			assert.Equal(t, tools.ActionDeny, got.Action)
			assert.Zero(t, cl.calls)
		})
	}

	t.Run("mcp_tool_still_classified", func(t *testing.T) {
		p := newFakePrompter()
		b := newTestBarrier(p)
		b.SetClassifier(&fakeClassifier{verdict: ClassAllow})
		b.SetMode(ModeAutoMCP)

		got := runAsk(b, t.Context(), "mcp__x", []byte(`{}`))
		assert.Equal(t, tools.ActionAllow, got.Action) // resolved by the classifier, no answer given
	})
}

func TestClassifyCall(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mode Mode
		tool string
		want bool
	}{
		{"auto_shell", ModeAuto, bashTool, true},
		{"auto_mcp_tool", ModeAuto, "mcp__x", false},
		{"autompc_mcp_tool", ModeAutoMCP, "mcp__x", true},
		{"autowrite_shell", ModeAutoWrite, bashTool, true},
		{"autowrite_mcp_tool", ModeAutoWrite, "mcp__x", true},
		{"allow_read_never", ModeAllowRead, bashTool, false},
		{"block_all_never", ModeBlockAll, bashTool, false},

		// a core writer is decided statically; the model never gets to call one read-only
		{"autompc_write", ModeAutoMCP, "write", false},
		{"autompc_edit", ModeAutoMCP, "edit", false},
		{"autowrite_write", ModeAutoWrite, "write", false},
		{"autowrite_edit", ModeAutoWrite, "edit", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newTestBarrier(nil)
			b.SetClassifier(&fakeClassifier{})
			assert.Equal(t, c.want, b.classifyCall(c.mode, c.tool))
		})
	}

	t.Run("nil_classifier_never", func(t *testing.T) {
		assert.False(t, newTestBarrier(nil).classifyCall(ModeAutoWrite, bashTool))
	})
}

func TestClassifySubject(t *testing.T) {
	t.Parallel()

	t.Run("shell_under_autowrite", func(t *testing.T) {
		s := classifySubject(ModeAutoWrite, bashCall("rm f"))
		assert.Equal(t, Subject{Name: bashTool, Args: "rm f", AllowWrite: true}, s)
	})
	t.Run("shell_under_auto", func(t *testing.T) {
		s := classifySubject(ModeAuto, bashCall("rm f"))
		assert.False(t, s.AllowWrite)
	})
	t.Run("mcp_never_allows_write", func(t *testing.T) {
		s := classifySubject(ModeAutoWrite, call("mcp__x", `{"a":1}`))
		assert.Equal(t, "mcp__x", s.Name)
		assert.False(t, s.AllowWrite) // the workspace rules are shell-only
	})
}

func TestCycleAdvancesModesInOrder(t *testing.T) {
	t.Parallel()

	b := newTestBarrier(newFakePrompter())
	want := []Mode{ModeAuto, ModeAutoMCP, ModeAutoWrite, ModeAllowAll, ModeBlockAll, ModeAllowRead}
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
