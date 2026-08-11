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
// (deduped against the tracker), directories inject an ls pair regardless of the
// tool's enabled state, and large/non-text files annotate in place. The byte
// cap pushes overflow to annotation. Re-expanding already-annotated text
// replaces the measurement rather than appending a second.
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
	// rebuild the text by splicing spans back to front so earlier offsets stay valid
	out := text
	for i := len(refs) - 1; i >= 0; i-- {
		ref := refs[i]
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
			before = prependDir(ctx, x, full, ref.Path, before)
			continue
		}
		if m.Kind != tools.KindText {
			out = splice(out, ref, annotate(ref, m))
			continue
		}
		// dedupe against an unchanged read this session
		if x.tracker != nil && x.tracker.Check(full) == nil {
			continue // already read and unchanged; literal stays
		}
		if overInjectLimit(m) || spent+int(m.Bytes) > tools.RefTotal.Bytes {
			out = splice(out, ref, annotate(ref, m))
			notices = append(notices, "@"+ref.Path+" too large; annotated")
			continue
		}
		// iterate back to front so earlier offsets stay valid; prepend each read
		// pair (as with dirs) so references land in forward transcript order.
		pair := injectRead(ctx, x, full, ref.Path)
		before = append(pair, before...)
		spent += int(m.Bytes)
	}
	return Result{Text: out, Before: before, Notices: notices}
}

// overInjectLimit reports whether m exceeds the per-file inject threshold.
func overInjectLimit(m tools.Measurement) bool {
	if tools.RefInject.Lines > 0 && m.Lines > tools.RefInject.Lines {
		return true
	}
	if tools.RefInject.Bytes > 0 && m.Bytes > int64(tools.RefInject.Bytes) {
		return true
	}
	return false
}

// injectRead runs a synthetic read call and returns its call + result pair.
func injectRead(ctx context.Context, x *Expander, full, display string) []llm.Message {
	tool, ok := x.reg.Lookup("read")
	if !ok {
		return nil
	}
	id := "ref-" + display
	input, _ := json.Marshal(map[string]any{"path": display})
	call := agent.ToolCall{ID: id, Name: "read", Input: input}
	out := agent.NewOutput(x.sink, id)
	done := x.sink.ToolStart(call, "read "+display)
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
			ID: id, Name: "read", Input: input}}},
		{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
			CallID: id, Content: res.Content, Display: res.Display, IsError: res.IsError}}},
	}
}

// prependDir injects an ls pair ahead of the current before slice, so multiple
// references land in transcript order.
func prependDir(ctx context.Context, x *Expander, full, display string, before []llm.Message) []llm.Message {
	tool, ok := x.reg.Lookup("ls")
	if !ok {
		return before
	}
	id := "ref-ls-" + display
	input, _ := json.Marshal(map[string]any{"path": display})
	call := agent.ToolCall{ID: id, Name: "ls", Input: input}
	out := agent.NewOutput(x.sink, id)
	done := x.sink.ToolStart(call, "ls "+display)
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
	pair := []llm.Message{
		{Role: llm.RoleAssistant, Content: llm.BlockList{llm.ToolCallBlock{
			ID: id, Name: "ls", Input: input}}},
		{Role: llm.RoleUser, Content: llm.BlockList{llm.ToolResultBlock{
			CallID: id, Content: res.Content, Display: res.Display, IsError: res.IsError}}},
	}
	return append(pair, before...)
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
