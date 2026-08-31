package subagent

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// maxContinueAttempts bounds the empty-summary nudges so a child that keeps
// withholding text cannot loop forever.
const maxContinueAttempts = 1

// minThinkingSummary is trimmed reasoning length that may stand in for a summary.
const minThinkingSummary = 200

// maxThinkingSummary bounds how much raw reasoning becomes the fallback summary,
// so an over-long chain-of-thought never bloats the parent context.
const maxThinkingSummary = 4000

// thinkingPreface heads a reasoning-only fallback so the parent knows what it read.
const thinkingPreface = "(sub-agent produced no summary; its internal reasoning follows)\n\n"

// errNoSummary is returned when neither text nor usable reasoning exists.
var errNoSummary = errors.New("sub-agent produced no output")

// run builds and drives one child agent, returning its final summary. It runs on
// the job's own goroutine with a per-job cancellable context.
func (m *Manager) run(ctx context.Context, j *job) (string, error) {
	model := m.model() // resolved at spawn so /settings applies to the next job
	ledger := j.tokens // child ledger created at Start, reused here and by poll payloads

	sink := newChildSink(j.id, j.num, func(key, text string, rank int) {
		if fn := m.opts.Activity; fn != nil {
			fn(key, text, rank)
		}
	})

	state := &agent.State{
		Model:     model,
		Reasoning: m.reasoning(),
		Tokens:    ledger,
	}

	var tools agent.ToolSet
	if src := m.opts.Tools; src != nil {
		tools = &toolSet{tools: childTools(src)}
	}

	a := agent.New(state, agent.Options{
		Provider:            m.opts.Provider,
		Tools:               tools,
		Sinks:               []agent.Sink{sink},
		Env:                 m.opts.Env,
		ProjectInstructions: m.opts.ProjectInstructions,
		SystemSnippets:      childSnippets(),
	})

	if err := a.Prompt(ctx, agent.Input{Text: taskPrompt(j.task, j.instructions)}); err != nil {
		return "", err
	}
	if ctx.Err() != nil { // an aborted context never yields a partial summary as done
		return "", ctx.Err()
	}

	last := lastAssistant(state.Messages)
	sum := assistantText(last)
	for attempt := 0; sum == "" && last != nil &&
		last.Stop != llm.StopError && last.Stop != llm.StopAborted &&
		attempt < maxContinueAttempts; attempt++ {
		if err := a.Prompt(ctx, agent.Input{Text: continueNudge}); err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		last = lastAssistant(state.Messages)
		sum = assistantText(last)
	}

	if sum == "" {
		if think := bestThinking(state.Messages); len([]rune(think)) >= minThinkingSummary {
			return thinkingPreface + strutil.Clip(think, maxThinkingSummary), nil
		}
		return "", errNoSummary
	}
	return strings.TrimSpace(sum), nil
}

// lastAssistant returns the most recent assistant message in msgs, or nil.
func lastAssistant(msgs []llm.Message) *llm.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == llm.RoleAssistant {
			return &msgs[i]
		}
	}
	return nil
}

// assistantText joins a message's non-empty text blocks, excluding thinking.
func assistantText(m *llm.Message) string {
	if m == nil {
		return ""
	}
	var parts []string
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// bestThinking returns the longest reasoning text among the trailing assistant
// messages that made no tool call, or "" when none carry any.
func bestThinking(msgs []llm.Message) string {
	var best string
	for i := len(msgs) - 1; i >= 0; i-- {
		m := &msgs[i]
		if m.Role != llm.RoleAssistant {
			continue // nudge inputs and tool-result turns sit between the answer turns
		}
		if slices.ContainsFunc(m.Content, func(b llm.Block) bool {
			_, ok := b.(llm.ToolCallBlock)
			return ok
		}) {
			break // only trailing answer turns qualify, never mid-investigation reasoning
		}
		if t := thinkingText(m); len(t) > len(best) {
			best = t
		}
	}
	return best
}

// thinkingText joins a message's non-empty thinking blocks.
func thinkingText(m *llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if tb, ok := b.(llm.ThinkingBlock); ok && strings.TrimSpace(tb.Text) != "" {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}
