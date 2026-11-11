package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// helpTestDocs is a fixed stand-in for the build-embedded doc set.
var helpTestDocs = []HelpDoc{
	{Name: "docs/features/cli.md", Body: "# CLI\n\nevery flag and the headless contract"},
	{Name: "README.md", Body: "# ajent\n\nreadme body"},
	{Name: "docs/features/permissions.md", Body: "# Permissions\n\nsafeCommands matching"},
}

// helpHoldProvider parks each Stream until released or ctx cancels, recording every
// request, so a test can interleave with an in-flight answer.
type helpHoldProvider struct {
	release chan struct{}
	text    string

	mu       sync.Mutex
	requests []llm.Request
}

func (p *helpHoldProvider) Name() string { return "hold" }

func (p *helpHoldProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	select {
	case <-p.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.SliceStream{Events: textTurn(p.text)}, nil
}

func (p *helpHoldProvider) seen() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return len(p.requests)
}

type helpRig struct {
	ctl *helpController
	ui  *hubUI
	log *emitLog
	st  *agent.State // the live session state the controller bills and reads
}

// newHelpRig wires a /help controller over ag/st with p answering, plus an event
// log for notice and print assertions.
func newHelpRig(t *testing.T, ag *agent.Agent, st *agent.State, ui *hubUI, p llm.Provider) helpRig {
	t.Helper()

	ctl := newHelpController(helpDeps{
		ag: ag, st: st, ui: ui,
		providerFor: func(llm.Model) (llm.Provider, error) { return p, nil },
		session:     "sess-help",
		docs:        helpTestDocs,
	})
	require.NotNil(t, ctl)
	log := &emitLog{}
	log.attach(ui.hub)
	return helpRig{ctl: ctl, ui: ui, log: log, st: st}
}

func TestNewHelpController(t *testing.T) {
	t.Parallel()

	model := llm.Model{ID: "m", Provider: "t"}
	st := &agent.State{Model: model, Tokens: tokens.New(model)}
	ag := agent.New(st, agent.Options{})
	ui := wrapUI(t, plainTestUI(t))
	deps := helpDeps{ag: ag, st: st, ui: ui,
		providerFor: func(llm.Model) (llm.Provider, error) { return &llm.ScriptedProvider{}, nil }}

	t.Run("no_docs_is_nil", func(t *testing.T) {
		assert.Nil(t, newHelpController(helpDeps{}))
		assert.Nil(t, newHelpController(deps))
	})

	t.Run("wired_builds_system_block", func(t *testing.T) {
		c := newHelpController(helpDeps{ag: ag, st: st, ui: ui,
			providerFor: deps.providerFor, docs: helpTestDocs})
		require.NotNil(t, c)
		assert.Contains(t, c.system, "<ajent_readme path=\"README.md\">")
	})
}

func TestHelpSystemBuild(t *testing.T) {
	t.Parallel()

	sys := helpSystem(canonicalDocs(helpTestDocs))

	readme := strings.Index(sys, `<ajent_readme path="README.md">`)
	cli := strings.Index(sys, `<ajent_features path="docs/features/cli.md">`)
	perms := strings.Index(sys, `<ajent_features path="docs/features/permissions.md">`)
	require.NotEqual(t, -1, readme)
	require.NotEqual(t, -1, cli)
	require.NotEqual(t, -1, perms)

	t.Run("readme_first_then_features_sorted", func(t *testing.T) {
		assert.Less(t, readme, cli)
		assert.Less(t, cli, perms)
	})

	t.Run("instructions_outside_the_tags", func(t *testing.T) {
		assert.Contains(t, sys, "only its own documentation")
		assert.Positive(t, strings.Index(sys, "<ajent_"))
	})
}

func TestHelpControllerAsk(t *testing.T) {
	t.Parallel()

	newRig := func(t *testing.T, p llm.Provider) helpRig {
		t.Helper()

		model := llm.Model{ID: "m", Provider: "t"}
		st := &agent.State{Model: model, Tokens: tokens.New(model)}
		ag := agent.New(st, agent.Options{
			Provider: func(llm.Model) (llm.Provider, error) { return p, nil },
		})
		return newHelpRig(t, ag, st, wrapUI(t, plainTestUI(t)), p)
	}

	t.Run("request_carries_docs_without_tools", func(t *testing.T) {
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: textTurn("docs answer")}}}
		rig := newRig(t, sp)

		rig.ctl.ask(t.Context(), "how do I mark tools read-only?")
		require.Eventually(t, func() bool { return len(rig.log.printsSeen()) == 1 }, time.Second, time.Millisecond)
		req := sp.Requests()[0]

		sys := llm.FirstBlockText(req.System)
		assert.Contains(t, sys, `<ajent_readme path="README.md">`)
		assert.Contains(t, sys, "readme body")
		assert.Contains(t, sys, "safeCommands matching")
		assert.NotContains(t, sys, "how do I mark tools read-only") // the question never joins the doc block
		assert.Empty(t, req.Tools)                                  // structurally tool-less

		require.Len(t, req.Messages, 1)
		assert.Equal(t, llm.RoleUser, req.Messages[0].Role)
		assert.Equal(t, "how do I mark tools read-only?", llm.JoinText(req.Messages[0].Content))

		prints := rig.log.printsSeen()
		require.Len(t, prints, 1)
		assert.Contains(t, prints[0], "docs answer")
		assert.Contains(t, prints[0], helpAnswerHeader)

		assert.Empty(t, rig.st.Messages) // live transcript untouched before and after
	})

	t.Run("warm_thread_shares_doc_block", func(t *testing.T) {
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: textTurn("first answer")},
			{Events: textTurn("second answer")},
		}}
		rig := newRig(t, sp)

		rig.ctl.ask(t.Context(), "one?")
		// wait for the answer itself: a request recorded is not an answer finished,
		// and asking again mid-answer is refused
		require.Eventually(t, func() bool { return len(rig.log.printsSeen()) == 1 }, time.Second, time.Millisecond)
		rig.ctl.ask(t.Context(), "two?")
		require.Eventually(t, func() bool {
			return len(rig.log.printsSeen()) == 2
		}, time.Second, time.Millisecond)

		reqs := sp.Requests()
		require.Len(t, reqs, 2)
		assert.Equal(t, reqs[0].System, reqs[1].System) // identical doc block, cacheable
		require.Len(t, reqs[1].Messages, 3)
		assert.Equal(t, "one?", llm.JoinText(reqs[1].Messages[0].Content))
		assert.Equal(t, llm.RoleAssistant, reqs[1].Messages[1].Role)
		assert.Equal(t, "first answer", llm.JoinText(reqs[1].Messages[1].Content))
		assert.Equal(t, "two?", llm.JoinText(reqs[1].Messages[2].Content))
	})

	t.Run("spend_bills_not_context", func(t *testing.T) {
		usage := llm.Usage{Input: 100, Output: 7}
		events := append([]llm.Event{{Type: llm.EventUsage, Usage: usage}}, textTurn("billed answer")...)
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: events}}}
		rig := newRig(t, sp)
		before := rig.st.Tokens.Context()

		rig.ctl.ask(t.Context(), "what does /usage show?")
		require.Eventually(t, func() bool {
			return len(rig.log.printsSeen()) == 1 // the answer settled
		}, time.Second, time.Millisecond)

		assert.Equal(t, usage.Output, rig.st.Tokens.Total().Output)
		assert.Equal(t, before, rig.st.Tokens.Context()) // bill-only: the bar does not move
	})

	t.Run("refuses_while_turn_runs", func(t *testing.T) {
		hp := &helpHoldProvider{release: make(chan struct{}), text: "turn reply"}
		rig := newRig(t, hp)
		release := sync.OnceFunc(func() { close(hp.release) })
		t.Cleanup(release)

		done := make(chan error, 1)
		go func() { done <- rig.ctl.deps.ag.Prompt(t.Context(), agent.Input{Text: "busy turn"}) }()
		require.Eventually(t, func() bool {
			return hp.seen() == 1 // the turn's request is up and streaming
		}, time.Second, time.Millisecond)

		rig.ctl.ask(t.Context(), "a help question?")
		require.Eventually(t, func() bool {
			return strings.Contains(strings.Join(rig.log.noticesSeen(), "\n"), "help refused")
		}, time.Second, time.Millisecond)
		assert.Equal(t, 1, hp.seen()) // no request left the controller

		release()
		require.NoError(t, <-done)
	})

	t.Run("refuses_second_concurrent", func(t *testing.T) {
		hp := &helpHoldProvider{release: make(chan struct{}), text: "held answer"}
		rig := newRig(t, hp)
		release := sync.OnceFunc(func() { close(hp.release) })
		t.Cleanup(release)

		rig.ctl.ask(t.Context(), "first?")
		require.Eventually(t, func() bool { return hp.seen() == 1 }, time.Second, time.Millisecond)

		rig.ctl.ask(t.Context(), "second?")
		require.Eventually(t, func() bool {
			return strings.Contains(strings.Join(rig.log.noticesSeen(), "\n"), "already running")
		}, time.Second, time.Millisecond)
		assert.Equal(t, 1, hp.seen())

		release()
		require.Eventually(t, func() bool { return len(rig.log.printsSeen()) == 1 }, time.Second, time.Millisecond)
	})

	t.Run("provider_error_not_answer", func(t *testing.T) {
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Err: errors.New("provider down")}}}
		rig := newRig(t, sp)

		rig.ctl.ask(t.Context(), "anything?")
		require.Eventually(t, func() bool {
			return strings.Contains(strings.Join(rig.log.noticesSeen(), "\n"), "help failed")
		}, time.Second, time.Millisecond)
		assert.Empty(t, rig.log.printsSeen()) // an error is never printed as an answer
	})

	t.Run("truncated_reports_cap_notice", func(t *testing.T) {
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Err: llm.ErrTruncated}}}
		rig := newRig(t, sp)

		rig.ctl.ask(t.Context(), "everything about ajent?")
		require.Eventually(t, func() bool {
			return strings.Contains(strings.Join(rig.log.noticesSeen(), "\n"), "length cap")
		}, time.Second, time.Millisecond)
		assert.Empty(t, rig.log.printsSeen()) // partial text never prints as an answer
	})

	t.Run("cancel_reports_notice", func(t *testing.T) {
		hp := &helpHoldProvider{release: make(chan struct{}), text: "never read"}
		rig := newRig(t, hp)
		t.Cleanup(func() { close(hp.release) })

		rig.ctl.ask(context.Background(), "slow one")
		require.Eventually(t, func() bool { return hp.seen() == 1 }, time.Second, time.Millisecond)

		require.True(t, rig.ctl.abort())
		require.Eventually(t, func() bool {
			return strings.Contains(strings.Join(rig.log.noticesSeen(), "\n"), "help cancelled")
		}, time.Second, time.Millisecond)
		assert.Empty(t, rig.log.printsSeen())
	})
}
