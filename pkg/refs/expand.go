package refs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

// Result is the outcome of expanding one message's @ references: the rewritten
// text (with annotations in place), the synthetic messages to append ahead of
// it, and any notices.
type Result struct {
	Text    string
	Before  []llm.Message
	Notices []string
}

// Expander turns @ references in a message into synthetic read/ls tool-call +
// result pairs, or annotates them in place when too large or non-text.
type Expander struct {
	reg     *tools.Registry
	sink    agent.Sink
	policy  tools.PathPolicy
	tracker *tools.Tracker
}

// NewExpander returns an expander backed by reg. read/ls run through the sink so
// their display order matches the transcript order. policy resolves @ paths to
// the same keys read/write/edit use.
func NewExpander(reg *tools.Registry, sink agent.Sink, policy tools.PathPolicy) *Expander {
	var tracker *tools.Tracker
	if reg != nil {
		tracker = reg.Tracker()
	}
	return &Expander{reg: reg, sink: sink, policy: policy, tracker: tracker}
}

// Expand resolves every @ reference in text. Small text files inject a read pair
// (deduped against the tracker), directories an ls pair regardless of enabled
// state, and large/non-text files annotate in place. Re-expanding an annotated
// ref replaces its measurement rather than appending a second.
func (x *Expander) Expand(ctx context.Context, text string) Result {
	refs := Parse(text)
	if len(refs) == 0 || x.reg == nil {
		return Result{Text: text}
	}
	var (
		before  []llm.Message
		notices []string
		spent   int
	)
	// splice back to front so earlier offsets stay valid; prepend each pair so
	// references land in forward transcript order.
	out := text
	for i := len(refs) - 1; i >= 0; i-- {
		ref := refs[i]
		// wildcard reference: list matching files via ls so the model sees which
		// paths matched before choosing what to read.
		if tools.HasGlob(ref.Path) {
			before = append(injectPair(ctx, x, "ls", "ref-ls-", ref.Path), before...)
			continue
		}
		full, err := x.policy.Resolve(ref.Path)
		if err != nil {
			notices = append(notices, "could not resolve @"+ref.Path+": "+err.Error())
			continue
		}
		m, err := tools.Measure(full)
		if err != nil {
			notices = append(notices, "@"+ref.Path+" not found")
			continue
		}
		if m.Dir {
			before = append(injectPair(ctx, x, "ls", "ref-ls-", ref.Path), before...)
			continue
		}
		if m.Kind != tools.KindText {
			out = splice(out, ref, annotate(ref, m))
			continue
		}
		// dedupe against an unchanged read this session; the literal stays
		if x.tracker != nil && x.tracker.Unchanged(full) {
			continue
		}
		rl := tools.RefInjectLimit()
		if overInjectLimit(m, rl) || spent+int(m.Bytes) > tools.RefTotalLimit().Bytes {
			out = splice(out, ref, annotate(ref, m))
			notices = append(notices, "@"+ref.Path+" too large; annotated")
			continue
		}
		before = append(injectPair(ctx, x, "read", "ref-", ref.Path), before...)
		spent += int(m.Bytes)
	}
	return Result{Text: out, Before: before, Notices: notices}
}

// overInjectLimit reports whether m exceeds the per-file inject threshold.
func overInjectLimit(m tools.Measurement, ref tools.Limit) bool {
	if ref.Lines > 0 && m.Lines > ref.Lines {
		return true
	}
	if ref.Bytes > 0 && m.Bytes > int64(ref.Bytes) {
		return true
	}
	return false
}

// injectPair runs a synthetic tool call and returns its call + result pair.
func injectPair(ctx context.Context, x *Expander, name, idPrefix, display string) []llm.Message {
	tool, ok := x.reg.Lookup(name)
	if !ok {
		return nil
	}
	id := idPrefix + display
	input, _ := json.Marshal(map[string]any{"path": display})
	call := agent.ToolCall{ID: id, Name: name, Input: input}
	out := agent.NewOutput(x.sink, id)
	done := x.sink.ToolStart(call, name+" "+display)
	res, err := tool.Execute(ctx, call, out)
	if res.Content == nil {
		res.Content = llm.BlockList{}
	}
	if err != nil && !res.IsError {
		res.IsError = true
		if len(res.Content) == 0 {
			res.Content = llm.BlockList{llm.TextBlock{Text: err.Error()}}
		}
	}
	done(res)
	return []llm.Message{
		{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ToolCallBlock{
			ID: id, Name: name, Input: input}}},
		{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
			CallID: id, Content: res.Content, Display: res.Display, IsError: res.IsError}}},
	}
}

// annotate returns the replacement text for a large/non-text reference: the
// @path plus its measurement. An existing annotation on ref is replaced, never
// doubled.
func annotate(ref Ref, m tools.Measurement) string {
	return "@" + ref.Path + " (" + measurementText(m) + ")"
}

// measurementText renders the annotation shape: "800 lines, 64kb" for text,
// "binary, 1.2mb" for binary, "image, 1.2mb" for images, "dir" for a directory.
func measurementText(m tools.Measurement) string {
	if m.Dir {
		return "dir"
	}
	switch m.Kind {
	case tools.KindBinary:
		return "binary, " + tools.HumanSize(m.Bytes)
	case tools.KindImage:
		return "image, " + tools.HumanSize(m.Bytes)
	default:
		if m.Lines > 0 {
			return fmt.Sprintf("%d lines, %s", m.Lines, tools.HumanSize(m.Bytes))
		}
		return tools.HumanSize(m.Bytes)
	}
}

// splice replaces the span of out covered by ref with repl.
func splice(out string, ref Ref, repl string) string {
	return out[:ref.Start] + repl + out[ref.End:]
}
