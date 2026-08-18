package compact

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/strutil"
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
[What is the user trying to accomplish? Can be multiple items.]

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
- [Data, examples, or references needed to continue]
- [(none) if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.
For content the assistant produced (code, prose, plans, answers), include a 2-3
sentence synopsis of its substance — never just a title or name.`

	initialInstruction = `The messages above are a conversation to summarize. Create a structured context
checkpoint that another model will use to continue the work.

` + sixSectionSpec

	incrementalInstruction = `The messages above are NEW conversation messages to incorporate into the existing
summary provided in <previous-summary>.

Update the structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context
- UPDATE Progress: move items In Progress → Done when completed
- UPDATE Next Steps based on what was accomplished
- PRESERVE exact file paths, function names, error messages

` + sixSectionSpec
)

// stage4 picks a cut point and summarises the span before it with run. It returns
// the summary text, the entry id to keep from and how many messages the summary
// covered; firstKept empty with a summary means a summary-only compaction. With
// opts.Force a cut always lands (keeping the current turn, or past the end when
// there is nothing older to fold); without it a missing cut skips the model call.
func stage4(ctx context.Context, branch []session.Entry, model llm.Model, run RunPrompt, opts Options) (summary, firstKept string, summarized int, err error) {
	target := resolveTarget(model, opts.TargetTokens)
	spanStart, _ := priorSpanStart(branch)

	cutIdx := selectCut(branch, target)
	if cutIdx < 0 || cutIdx <= spanStart {
		if !opts.Force {
			return "", "", 0, nil // no usable cut; stages 1-3 alone are the plan
		}
		// forced: keep the current turn when an older one can fold, else fold all
		if cutIdx = lastUserTurn(branch); cutIdx <= spanStart || countMessages(branch[spanStart:cutIdx]) == 0 {
			cutIdx = len(branch)
		}
	}
	if cutIdx < 0 || cutIdx > len(branch) {
		return "", "", 0, nil
	}

	end := cutIdx // summarise only what the cut drops, never the kept tail
	if cutIdx < len(branch) {
		if e := &branch[cutIdx]; e.Type == session.TypeMessage {
			firstKept = e.ID
		}
	}
	// cutIdx == len(branch): keep nothing; a summary-only compaction

	summarisable := summarisableEntries(branch, spanStart, end)
	if len(summarisable) == 0 {
		return "", firstKept, 0, nil // nothing new to summarise; cut alone is the plan
	}

	var prev string
	for i := range branch {
		if branch[i].Type != session.TypeCompaction {
			continue
		}
		var cd session.CompactionData
		if err := branch[i].Decode(&cd); err == nil && cd.Summary != "" {
			prev = cd.Summary // newest prior summary merges into this one
		}
	}

	var spanTokens int
	for _, e := range summarisable {
		spanTokens += entryTokens(e)
	}

	prompt := buildPrompt(summarisable, prev, opts.Instructions)
	req := llm.Request{
		Model:     model,
		System:    llm.BlockList{llm.TextBlock{Text: summarizerSystem}},
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: prompt}}}},
		MaxTokens: summarizeBudget(model, spanTokens),
	}
	out, err := run(ctx, req)
	if err != nil {
		return "", firstKept, 0, err
	}
	summary = strings.TrimSpace(out)
	if summary == "" {
		return "", firstKept, 0, errors.New("summariser returned an empty summary")
	}
	return summary, firstKept, countMessages(summarisable), nil
}

// summarisableEntries returns the message entries in [spanStart, end) that a new
// summary should cover.
func summarisableEntries(branch []session.Entry, spanStart, end int) []session.Entry {
	if spanStart < 0 {
		spanStart = 0
	}
	if end > len(branch) {
		end = len(branch)
	}
	if spanStart >= end {
		return nil
	}
	return branch[spanStart:end]
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
// a previous summary when merging, and any /compact focus instruction.
func buildPrompt(entries []session.Entry, prev, instructions string) string {
	var b strings.Builder
	b.WriteString("<conversation>\n")
	serialise(&b, entries)
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
// data rather than a live thread.
func serialise(b *strings.Builder, entries []session.Entry) {
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
					fmt.Fprintf(b, "[Tool result]: %s\n", strutil.Clip(resultText(tr), 2048))
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
				case llm.ThinkingBlock:
					if strings.TrimSpace(c.Text) != "" {
						b.WriteString("[Assistant thinking]: " + strutil.Clip(c.Text, 1024) + "\n")
					}
				case llm.ToolCallBlock:
					fmt.Fprintf(b, "[Assistant tool calls]: %s(%s)\n", c.Name, strutil.Clip(string(c.Input), 512))
				}
			}
		default:
			// system and other roles are not part of the transcript summary
		}
	}
}

// summarizeBudget sizes the summariser output for a span of spanTokens: a
// fraction of the response reserve, capped below the span so a summary can
// always come out smaller than what it replaces.
func summarizeBudget(model llm.Model, spanTokens int) int {
	reserve := tokensReserve(model)
	maxOut := model.MaxOutput
	if maxOut <= 0 {
		maxOut = reserve
	}
	budget := min(reserve*8/10, maxOut)          // 80% of the response reservation
	budget = min(budget, max(256, spanTokens/2)) // below the span it replaces
	return max(budget, 256)
}

// lastUserTurn returns the branch index of the most recent real user prompt, or
// -1 when the branch has none.
func lastUserTurn(branch []session.Entry) int {
	for i := len(branch) - 1; i >= 0; i-- {
		e := &branch[i]
		if e.Type != session.TypeMessage {
			continue
		}
		var md session.MessageData
		if err := e.Decode(&md); err != nil || md.Message.Role != llm.RoleUser {
			continue
		}
		if !llm.OnlyToolResults(md.Message.Content) {
			return i
		}
	}
	return -1
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
