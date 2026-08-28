package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// Summariser prompts, from docs/prompt-design.md. They are the exact-format spec
// that makes a checkpoint resumable: goal, constraints, progress, decisions,
// next steps and critical context with file paths preserved verbatim.
const (
	summarizerSystem = `You are a context summarization assistant. Your task is to read a conversation
between a user and an AI assistant, then produce a structured summary following
the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in it.
ONLY output the structured summary.`

	sixSectionSpec = `Use this EXACT format:

## Goal
[The objective as it now stands. If the user redirected the work, state the
current objective and note what changed.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements]
- [(none) if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Everything needed to continue without re-reading the history: exact file paths
  and what changed in each, error text, command lines and their outcomes, API and
  type shapes the work relies on, values discovered by investigation, and
  approaches already ruled out with the reason they were ruled out.]
- [(none) if not applicable]

Be brief in wording and complete in substance. Cut preamble, hedging and
adjectives; never cut a fact. Preserve file paths, function names, error messages
and command lines exactly as written. For content the assistant produced (code,
prose, plans, answers), include a 2-3 sentence synopsis of its substance — never
just a title or name.`

	// excludedTail tells the summariser its span deliberately stops short of the
	// present, so it stops writing Next Steps as though its last message were the
	// latest thing that happened.
	excludedTail = `The most recent steps are deliberately NOT shown above: they are kept verbatim
and follow your summary. Summarise only what you were given; the reader can see
newer activity than you can.`

	initialInstruction = `The messages above are a conversation to summarize. Create a structured context
checkpoint that another model will use to continue the work.

` + excludedTail + `

` + sixSectionSpec

	incrementalInstruction = `The messages above are NEW conversation messages to incorporate into the existing
summary provided in <previous-summary>.

` + excludedTail + `

Produce ONE merged summary. RULES:
- INTEGRATE the previous summary rather than appending to it; never state the same
  fact twice
- REPLACE the goal if the user redirected the work, noting what changed
- UPDATE Progress: move items In Progress → Done as work completed
- COMPRESS finished work to one line per item once nothing depends on its detail
- NEVER drop a constraint, a decision, or outstanding work
- PRESERVE exact file paths, function names, error messages and command lines

` + sixSectionSpec
)

// minSummaryTokens floors a merged checkpoint's budget.
const minSummaryTokens = 8192 // a merged checkpoint is never amputated by a hard cap

// summarise folds the message entries in [spanStart, end) into a checkpoint with
// run, merging any previous summary on the branch. It returns the summary text and
// how many messages it covered; an empty summary with no error means there was
// nothing new to fold. stubs are replacement markers for the span, applied so the
// summariser reads what compaction already reduced rather than raw output.
func summarise(ctx context.Context, branch []session.Entry, spanStart, end int, stubs []session.Stub, model llm.Model, run RunPrompt, opts Options) (summary string, summarized int, err error) {
	prev := priorSummary(branch)
	if spanStart < 0 {
		spanStart = 0
	}
	if end > len(branch) {
		end = len(branch)
	}
	if spanStart >= end { // nothing new to fold
		return "", 0, nil
	}
	entries := branch[spanStart:end]

	var span int
	for _, e := range entries {
		span += entryTokens(e)
	}

	maxOut := summarizeBudget(model, span, tokens.EstimateText(prev, tokens.KindProse))
	prompt, kept, err := fitPrompt(entries, prev, opts.Instructions, stubs, model, maxOut)
	if err != nil {
		return "", 0, err
	}
	req := llm.Request{
		Model:     model,
		System:    llm.BlockList{llm.TextBlock{Text: summarizerSystem}},
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: prompt}}}},
		MaxTokens: maxOut,
	}
	out, err := run(ctx, req)
	if err != nil {
		return "", 0, err
	}
	summary = strings.TrimSpace(out)
	if summary == "" {
		return "", 0, errors.New("summariser returned an empty summary")
	}
	return summary, kept, nil
}

// priorSummary returns the newest summary recorded on the branch, for merging.
func priorSummary(branch []session.Entry) string {
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type != session.TypeCompaction {
			continue
		}
		var cd session.CompactionData
		if err := branch[i].Decode(&cd); err == nil && cd.Summary != "" {
			return cd.Summary
		}
	}
	return ""
}

// countMessages reports how many message entries a span holds, for the notice.
func countMessages(entries []session.Entry) int {
	n := 0
	for _, e := range entries {
		if e.Type == session.TypeMessage {
			n++
		}
	}
	return n
}

// buildPrompt assembles one user message for the summariser: conversation tags,
// a previous summary when merging, and any /compact focus instruction. dropped
// notes inside the tags that the transcript is missing its oldest entries.
func buildPrompt(entries []session.Entry, prev, instructions string, stubs []session.Stub, clip int, dropped bool) string {
	var b strings.Builder
	b.WriteString("<conversation>\n")
	if dropped {
		b.WriteString("[earlier messages omitted]\n")
	}
	byCall := make(map[string]session.Stub, len(stubs))
	for _, s := range stubs {
		byCall[s.CallID] = s
	}
	serialise(&b, entries, byCall, clip)
	b.WriteString("</conversation>\n\n")

	instr := initialInstruction
	if prev != "" {
		b.WriteString("<previous-summary>\n" + prev + "\n</previous-summary>\n\n")
		instr = incrementalInstruction
	}
	b.WriteString(instr)
	if strings.TrimSpace(instructions) != "" {
		fmt.Fprintf(&b, "\n\nAdditional focus: %s", instructions)
	}
	return b.String()
}

// serialise flattens message entries to a text transcript the summariser reads as
// data rather than a live thread, substituting any stub for its result. Thinking
// is left out entirely and tool output is clipped to clip runes (0 for no clip);
// user and assistant prose is never clipped, being the semantic payload.
func serialise(b *strings.Builder, entries []session.Entry, stubs map[string]session.Stub, clip int) {
	for _, e := range entries {
		if e.Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		if err := e.Decode(&md); err != nil {
			continue
		}
		m := md.Message
		switch m.Role {
		case llm.RoleUser:
			for _, blk := range m.Content {
				if tr, ok := blk.(llm.ToolResultBlock); ok {
					fmt.Fprintf(b, "[Tool result]: %s\n", clipTo(stubbedText(tr, stubs), clip))
				}
			}
			if t := userPlain(m); t != "" {
				b.WriteString("[User]: " + t + "\n")
			}
		case llm.RoleAssistant:
			for _, blk := range m.Content {
				switch c := blk.(type) {
				case llm.TextBlock:
					if strings.TrimSpace(c.Text) != "" {
						b.WriteString("[Assistant]: " + c.Text + "\n")
					}
				case llm.ToolCallBlock:
					fmt.Fprintf(b, "[Assistant tool calls]: %s(%s)\n", c.Name, clipTo(string(c.Input), capCallInput(clip)))
				}
			}
		default:
			// system and other roles are not part of the transcript summary
		}
	}
}

// stubbedText returns what a tool result contributes to the transcript: its stub
// replacement when the plan has one.
func stubbedText(tr llm.ToolResultBlock, stubs map[string]session.Stub) string {
	text := resultText(tr)
	s, ok := stubs[tr.CallID]
	if ok && s.Text != "" {
		return s.Text
	}
	return text
}

// clipLadder is tried in order until the transcript fits the model window. Zero
// keeps tool output whole, which is the normal outcome: serialisation already
// compresses a branch several-fold before any clipping.
var clipLadder = []int{0, 8192, 4096, 2048, 1024, 512}

// clipTo truncates to n runes, or returns s whole when n is not positive.
func clipTo(s string, n int) string {
	if n <= 0 {
		return s
	}
	return strutil.Clip(s, n)
}

// capCallInput bounds a tool call's JSON input, which is argument shape rather
// than output and never needs the full allowance.
func capCallInput(clip int) int {
	if clip <= 0 || clip > 512 {
		return 512
	}
	return clip
}

// fitPrompt builds the summariser prompt at the largest clip that leaves room for
// a maxOut-token reply. Clipping is a safety valve, not a default: compaction
// fires near the top of the window, so a span with no structural compressibility
// plus its summary can overflow, and an oversized request would fail the session
// exactly when it most needs to shrink.
func fitPrompt(entries []session.Entry, prev, instructions string, stubs []session.Stub, model llm.Model, maxOut int) (prompt string, kept int, err error) {
	avail := promptBudget(model, maxOut)
	fits := func(p string) bool {
		return avail <= 0 || tokens.EstimateText(p, tokens.KindCode) <= avail
	}

	tightest := clipLadder[len(clipLadder)-1]
	for _, clip := range clipLadder { // the first rung keeps output whole
		prompt = buildPrompt(entries, prev, instructions, stubs, clip, false)
		if fits(prompt) {
			return prompt, countMessages(entries), nil // the common case returns on the first build
		}
	}

	// even the tightest clip busts: drop the oldest entries before giving up,
	// rather than send a request the provider will reject. Dropping must never
	// empty the transcript into a "summary of nothing" prompt.
	for len(entries) > 0 {
		entries = entries[len(entries)/4+1:]
		if len(entries) == 0 { // exhausted; fall through to the clipped-prior tail
			break
		}
		prompt = buildPrompt(entries, prev, instructions, stubs, tightest, true)
		if fits(prompt) {
			return prompt, countMessages(entries), nil
		}
	}
	if prev != "" { // a clipped checkpoint still merges; a rejected request does not
		prompt = buildPrompt(entries, strutil.Clip(prev, max(avail/2, 256)), instructions, stubs, tightest, true)
		if fits(prompt) {
			return prompt, countMessages(entries), nil
		}
	}
	return "", 0, errors.New("compact: summariser prompt does not fit the model window")
}

// promptBudget reports how many tokens the summariser user message may occupy:
// the window less the reply, the system block that rides beside it, and a margin
// for estimator error. Only the system block is subtracted — the instruction and
// any previous summary live inside the message being measured. Zero means the
// window is unknown and no bound applies.
func promptBudget(model llm.Model, maxOut int) int {
	if model.ContextWindow <= 0 {
		return 0
	}
	system := tokens.EstimateText(summarizerSystem, tokens.KindProse)
	margin := max(512, model.ContextWindow/64)
	return model.ContextWindow - maxOut - system - margin
}

// summarizeBudget sizes the summariser output for a span of span tokens plus prev:
// capped by what the model can emit and by everything the call replaces (the prior
// summary folded in), and at least minSummaryTokens so a merged checkpoint is never
// amputated, floored against a quarter of the compaction point.
func summarizeBudget(model llm.Model, span, prev int) int {
	emitCap := model.MaxOutput
	if emitCap <= 0 {
		emitCap = model.Reserve()
	}
	return min(emitCap,
		max(minSummaryTokens, compactAt(model)/4),
		max(minSummaryTokens, span+prev))
}

// userPlain extracts the plain text blocks of a prompt message.
func userPlain(m llm.Message) string {
	var parts []string
	for _, b := range m.Content {
		if tb, ok := b.(llm.TextBlock); ok && strings.TrimSpace(tb.Text) != "" {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, "\n")
}
