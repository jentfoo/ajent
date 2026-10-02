package compact

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/go-analyze/bulk"
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
prose, plans, answers), include a 2-3 sentence synopsis of its substance, never just
a title or name.`

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
// how many messages it covered. An empty summary with no error means there was
// nothing new to fold. stubs are replacement markers for the span, applied so the
// summariser reads what compaction already reduced rather than raw output.
func summarise(ctx context.Context, v *branchView, spanStart, end int, stubs []session.Stub, model llm.Model, run RunPrompt, opts Options) (string, int, error) {
	prev := priorSummary(v.branch)
	if spanStart < 0 {
		spanStart = 0
	}
	if end > len(v.branch) {
		end = len(v.branch)
	}
	if spanStart >= end { // nothing new to fold
		return "", 0, nil
	}
	return summariseRegion(ctx, v, spanStart, end, prev, stubs, model, run, opts)
}

// summariseRegion folds [start, end) into a checkpoint, walking left to right
// with one run call per chunk that fits, chaining each summary into the next as
// prev. It returns the final summary and how many messages the chain covered, so
// nothing is cut from context without reaching the summary.
func summariseRegion(ctx context.Context, v *branchView, start, end int, prev string, stubs []session.Stub, model llm.Model, run RunPrompt, opts Options) (string, int, error) {
	var kept int
	merge := prev
	for from := start; from < end; {
		span := v.spanTokens(from, end)
		maxOut := summarizeBudget(model, span, tokens.EstimateText(merge, tokens.KindProse))
		plan, err := v.fitPrompt(from, end, merge, opts.Instructions, stubs, model, maxOut)
		if err != nil {
			return "", 0, err
		}
		if plan.from > from {
			// the oldest chunk does not fit alongside the rest: fold it in first
			var nsum int
			if merge, nsum, err = summariseRegion(ctx, v, from, plan.from, merge, stubs, model, run, opts); err != nil {
				return "", 0, err
			}
			kept += nsum
			from = plan.from
			continue
		}

		prompt := buildPrompt(v, from, end, plan.prev, opts.Instructions, stubs, plan.clip, from > start)
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
		summary := strings.TrimSpace(out)
		if summary == "" {
			return "", 0, errors.New("summariser returned an empty summary; retry, or if context usage is low skip compaction")
		}
		return summary, kept + v.countMessages(from, end), nil
	}
	return merge, kept, nil // unreachable: every pass either returns or advances from
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

// buildPrompt assembles one user message for the summariser: conversation tags,
// a previous summary when merging, and any /compact focus instruction. dropped
// notes inside the tags that the transcript is missing its oldest entries.
func buildPrompt(v *branchView, start, end int, prev, instructions string, stubs []session.Stub, clip int, dropped bool) string {
	var b strings.Builder
	b.WriteString("<conversation>\n")
	if dropped {
		b.WriteString("[earlier messages omitted]\n")
	}
	byCall := bulk.SliceToIndexBy(func(s session.Stub) string { return s.CallID }, stubs)
	serialise(&b, v, start, end, byCall, clip)
	b.WriteString("</conversation>\n\n")

	instr := initialInstruction
	if prev != "" {
		b.WriteString("<previous-summary>\n" + prev + "\n</previous-summary>\n\n")
		instr = incrementalInstruction
	}
	b.WriteString(instr)
	if strings.TrimSpace(instructions) != "" {
		_, _ = fmt.Fprintf(&b, "\n\nAdditional focus: %s", instructions)
	}
	return b.String()
}

// serialise flattens message entries to a text transcript the summariser reads as
// data rather than a live thread, substituting any stub for its result. Thinking
// is left out entirely and tool output is clipped to clip runes (math.MaxInt
// for no clip). User and assistant prose is never clipped, being the semantic
// payload.
func serialise(b *strings.Builder, v *branchView, start, end int, stubs map[string]session.Stub, clip int) {
	for i := start; i < end; i++ {
		md, ok := v.message(i)
		if !ok {
			continue
		}
		m := md.Message
		switch m.Role {
		case llm.RoleUser:
			for _, blk := range m.Content {
				if tr, ok := blk.(llm.ToolResultBlock); ok {
					_, _ = fmt.Fprintf(b, "[Tool result]: %s\n", strutil.Clip(stubbedText(tr, stubs), clip))
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
					_, _ = fmt.Fprintf(b, "[Assistant tool calls]: %s(%s)\n", c.Name, strutil.Clip(string(c.Input), capCallInput(clip)))
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

// clipLadder is tried in order until the transcript fits the model window.
// MaxInt keeps tool output whole, which is the normal outcome: serialisation
// already compresses a branch several-fold before any clipping.
var clipLadder = []int{math.MaxInt, 8192, 4096, 2048, 1024, 512}

// capCallInput bounds a tool call's JSON input, which is argument shape rather
// than output and never needs the full allowance.
func capCallInput(clip int) int {
	if clip > 512 {
		return 512
	}
	return clip
}

// fitPlan is the sizing fitPrompt settled on.
type fitPlan struct {
	clip int    // serialisation clip for the span
	from int    // span start after any oldest-entry drop
	prev string // prior summary to merge, clipped when nothing else fits
}

// fitPrompt sizes the summariser prompt: the largest clip that leaves room for a
// maxOut-token reply, dropping the oldest entries and finally clipping prev when
// even the tightest clip busts. from never reaches end, so the transcript the
// summariser reads is never empty.
func (v *branchView) fitPrompt(spanStart, end int, prev, instructions string, stubs []session.Stub, model llm.Model, maxOut int) (fitPlan, error) {
	avail := promptBudget(model, maxOut)
	fits := func(p string) bool {
		return avail <= 0 || tokens.EstimateText(p, tokens.KindCode) <= avail
	}

	for _, clip := range clipLadder { // the first rung keeps output whole
		if fits(buildPrompt(v, spanStart, end, prev, instructions, stubs, clip, false)) {
			return fitPlan{clip: clip, from: spanStart, prev: prev}, nil
		}
	}

	// even the tightest clip busts: drop the oldest entries before giving up,
	// rather than send a request the provider will reject.
	tightest := clipLadder[len(clipLadder)-1]
	if from, ok := v.dropToFit(spanStart, end, prev, instructions, stubs, tightest, fits); ok {
		return fitPlan{clip: tightest, from: from, prev: prev}, nil
	}
	if prev != "" { // a clipped checkpoint still merges, while a rejected request does not
		clipped := strutil.Clip(prev, max(avail/2, 256))
		if from, ok := v.dropToFit(spanStart, end, clipped, instructions, stubs, tightest, fits); ok {
			return fitPlan{clip: tightest, from: from, prev: clipped}, nil
		}
	}
	return fitPlan{}, errors.New("summariser prompt does not fit the model window")
}

// dropToFit advances the span start a quarter of the remainder at a time until
// the survivors-only prompt fits, false when nothing would be left to summarise.
func (v *branchView) dropToFit(start, end int, prev, instructions string, stubs []session.Stub, clip int, fits func(string) bool) (int, bool) {
	for from := start; from < end; {
		from += (end-from)/4 + 1
		if from >= end { // exhausted: dropping everything is not a summary
			return 0, false
		}
		if fits(buildPrompt(v, from, end, prev, instructions, stubs, clip, true)) {
			return from, true
		}
	}
	return 0, false
}

// promptBudget reports how many tokens the summariser user message may occupy:
// the window less the reply, the system block that rides beside it, and a margin
// for estimator error. Only the system block is subtracted, because the instruction
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
