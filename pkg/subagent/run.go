package subagent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	osexec "os/exec"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// maxContinueAttempts bounds the wrap-up nudges so a child that keeps
// withholding or truncating its summary cannot loop forever.
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

// errTruncated is returned when the final message was still cut short by an
// output or step cap after the nudge, so partial work never reads as complete.
var errTruncated = errors.New("sub-agent was cut off before a summary")

// gitInWorkTree reports whether cwd is inside a git work tree. It shells out
// to system git on purpose: the answer only decides whether the (go-git based,
// subprocess-free) git tools are offered, and rev-parse executes nothing from
// the repository. Same check as tools.IsGitRepo, which this package may not
// import.
func gitInWorkTree(ctx context.Context, cwd string) bool {
	if cwd == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := osexec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// run builds and drives one child agent, returning its final summary. It runs on
// the job's own goroutine with a per-job cancellable context.
func (m *Manager) run(ctx context.Context, j *job) (string, error) {
	model := m.model() // resolved at spawn so /settings applies to the next job
	ledger := j.tokens // child ledger created at Start, reused here and by poll payloads
	// Child() pins the parent's model; rebase so window/reserve follow the child's own
	ledger.SetWindow(model)

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
	src := m.opts.Tools
	inRepo := src != nil && gitInWorkTree(ctx, m.opts.Env.Cwd)
	if src != nil {
		tools = newToolSet(childTools(src, inRepo))
	}

	a := agent.New(state, agent.Options{
		Provider:            m.opts.Provider,
		Tools:               tools,
		Sinks:               []agent.Sink{sink},
		Env:                 m.opts.Env,
		ProjectInstructions: m.opts.ProjectInstructions,
		SystemSnippets:      childSnippets(inRepo),
		MaxSteps:            m.opts.MaxSteps, // bounds a runaway investigation
	})

	if err := a.Prompt(ctx, agent.Input{Text: taskPrompt(j.task, j.instructions)}); err != nil {
		return "", err
	}
	if ctx.Err() != nil { // an aborted context never yields a partial summary as done
		return "", ctx.Err()
	}

	last := lastAssistant(state.Messages)
	sum := assistantText(last)
	for attempt := 0; needsNudge(sum, last) && attempt < maxContinueAttempts; attempt++ {
		if err := a.Prompt(ctx, agent.Input{Text: nudgeFor(last)}); err != nil {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		last = lastAssistant(state.Messages)
		sum = assistantText(last)
	}

	// still cut short after the wrap-up turn: partial work never reads as done.
	if truncated(last) {
		return "", errTruncated
	}
	if sum == "" {
		if think := bestThinking(state.Messages); len([]rune(think)) >= minThinkingSummary {
			return thinkingPreface + strutil.Clip(think, maxThinkingSummary), nil
		}
		return "", errNoSummary
	}
	return strings.TrimSpace(sum), nil
}

// needsNudge reports whether the final message fails the summary contract and a
// wrap-up turn may still fix it: blank text, or output cut short by an output
// or step cap. A stop that is an error or abort is never nudged.
func needsNudge(sum string, last *llm.Message) bool {
	if last == nil || last.Stop == llm.StopError || last.Stop == llm.StopAborted {
		return false
	}
	return sum == "" || truncated(last)
}

// truncated reports whether a message cannot be the final summary: cut short by
// an output cap, or still carrying tool calls because the step limit ended the
// turn mid-investigation (the loop reports that stop as StopMaxTokens on the
// turn result without stamping it on the message).
func truncated(m *llm.Message) bool {
	return m != nil && (m.Stop == llm.StopMaxTokens || hasToolCall(m))
}

// hasToolCall reports whether a message carries any tool-call block.
func hasToolCall(m *llm.Message) bool {
	return m != nil && slices.ContainsFunc(m.Content, func(b llm.Block) bool {
		_, ok := b.(llm.ToolCallBlock)
		return ok
	})
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
	var sb strings.Builder
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteRune('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
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
	var sb strings.Builder
	for _, b := range m.Content {
		if tb, ok := b.(llm.ThinkingBlock); ok && strings.TrimSpace(tb.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteRune('\n')
			}
			sb.WriteString(tb.Text)
		}
	}
	return sb.String()
}
