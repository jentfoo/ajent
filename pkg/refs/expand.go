package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// idPrefix leads every injected @ reference call id.
const idPrefix = "ref-"

// Result is the outcome of expanding one message's @ references: the rewritten
// text (with annotations in place), a Run that produces the synthetic messages
// to append behind it, and any notices.
type Result struct {
	Text    string
	Est     int // estimated tokens the pending injections add
	Notices []string
	// Run executes the planned reads and returns their call + result pairs in
	// document order. It is nil when nothing is injected.
	Run func(context.Context) []llm.Message
}

// injection is one planned read/ls call, run once the user message lands.
type injection struct {
	name  string // read or ls
	id    string
	path  string          // as written, what the call is given
	full  string          // resolved, for the land-time tracker re-check; empty for ls
	input json.RawMessage // the call's arguments, marshalled at plan time
	body  int64           // expected result bytes, for the submit reserve
}

// estimate sizes the call + result pair this injection lands. It bills the same
// framing EstimateMessages charges once the pair is appended, so the bar does not
// step as the reads arrive.
func (p injection) estimate() int {
	return tokens.EstimateToolPair(
		llm.ToolCallBlock{ID: p.id, Name: p.name, Input: p.input}, p.body, tokens.KindCode)
}

// newInjection plans one call, marshalling its arguments now so the reserve and
// the landed pair are sized from exactly the same call.
func newInjection(run int64, name, idKind, path, full string, body int64) injection {
	input, _ := json.Marshal(map[string]any{"path": path})
	return injection{name: name, id: callID(run, idKind, path), path: path,
		full: full, input: input, body: body}
}

// Expander turns @ references in a message into synthetic read/ls tool-call +
// result pairs, or annotates them in place when too large or non-text.
type Expander struct {
	reg     *tools.Registry
	sink    agent.Sink
	policy  tools.PathPolicy
	tracker *tools.Tracker
	run     atomic.Int64 // numbers each Expand so its call ids stay unique
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

// Seed raises the run counter above every @ reference id already in msgs. Call
// it whenever the context is rebuilt (resume, rewind, fork) so a later Expand
// never mints an id the context already holds.
func (x *Expander) Seed(msgs []llm.Message) {
	var high int64
	for _, m := range msgs {
		for _, blk := range m.Content {
			call, ok := blk.(llm.ToolCallBlock)
			if !ok {
				continue
			}
			if n := runOf(call.ID); n > high {
				high = n
			}
		}
	}
	for {
		cur := x.run.Load()
		if cur >= high || x.run.CompareAndSwap(cur, high) {
			return
		}
	}
}

// runOf returns the run number encoded in an injected call id, or 0.
func runOf(id string) int64 {
	rest, ok := strings.CutPrefix(id, idPrefix)
	if !ok {
		return 0
	}
	digits, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Expand plans every @ reference in text. Small text files plan a read pair
// (deduped against the tracker and against the rest of the message), directories
// an ls pair regardless of enabled state, and large/non-text files annotate in
// place. Re-expanding an annotated ref replaces its measurement rather than
// appending a second.
func (x *Expander) Expand(text string) Result {
	refs := Parse(text)
	if len(refs) == 0 || x.reg == nil {
		return Result{Text: text}
	}
	run := x.run.Add(1)
	var (
		plan    []injection
		notices []string
		spent   int
	)
	seen := make(map[string]struct{}, len(refs))
	// one injection per path however often the message names it: a repeat would
	// otherwise duplicate both the content and its call id
	keep := func(key string) bool {
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		return true
	}
	// splice back to front so earlier offsets stay valid; the plan is reversed
	// afterwards so references land in forward transcript order.
	out := text
	for i := len(refs) - 1; i >= 0; i-- {
		ref := refs[i]
		// wildcard reference: list matching files via ls so the model sees which
		// paths matched before choosing what to read.
		if tools.HasGlob(ref.Path) {
			if keep("ls:" + ref.Path) { // patterns never resolve; dedupe as written
				plan = append(plan, lsCall(run, ref.Path))
			}
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
			if keep("ls:" + full) {
				plan = append(plan, lsCall(run, ref.Path))
			}
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
		if !keep(full) {
			continue
		}
		// the tool returns line-numbered text, so what lands is the file plus a
		// prefix per line, not its raw size
		plan = append(plan, newInjection(run, "read", "", ref.Path, full, tools.ReadBytes(m)))
		spent += int(m.Bytes)
	}
	// sized here rather than per branch: an injection that forgot to add its own
	// cost is exactly how ls came to reserve nothing at all
	var est int
	for _, p := range plan {
		est += p.estimate()
	}
	res := Result{Text: out, Est: est, Notices: notices}
	if len(plan) > 0 {
		slices.Reverse(plan)
		res.Run = func(ctx context.Context) []llm.Message {
			msgs := make([]llm.Message, 0, 2*len(plan)) // a call + result per injection
			for _, p := range plan {
				if ctx.Err() != nil {
					break // an interrupt stops the batch rather than injecting error pairs
				}
				// re-checked here, not just at plan time: another message in the same
				// batch may have read it since
				if p.full != "" && x.tracker != nil && x.tracker.Unchanged(p.full) {
					continue
				}
				msgs = append(msgs, x.injectPair(ctx, p)...)
			}
			return msgs
		}
	}
	return res
}

// lsNominalBytes is the listing size an ls injection reserves. A directory or
// glob cannot be measured without doing the walk, which is too much to pay on the
// input path, so this is a floor rather than a measurement: the real size lands
// with the pair and replaces it moments later.
const lsNominalBytes = 1024

// lsCall plans the directory listing for a glob or directory reference.
func lsCall(run int64, path string) injection {
	return newInjection(run, "ls", "ls-", path, "", lsNominalBytes)
}

// callID names one injected reference. The run number is what keeps a later
// reference to the same path from reusing an id the context already holds.
func callID(run int64, kind, path string) string {
	return fmt.Sprintf("%s%d-%s%s", idPrefix, run, kind, path)
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

// injectPair runs one planned call and returns its call + result pair.
func (x *Expander) injectPair(ctx context.Context, p injection) []llm.Message {
	tool, ok := x.reg.Lookup(p.name)
	if !ok {
		return nil
	}
	msgs, _ := agent.InjectPair(ctx, tool, x.sink,
		agent.ToolCall{ID: p.id, Name: p.name, Input: p.input}, p.name+" "+p.path)
	return msgs
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
		return "binary, " + strutil.HumanSize(m.Bytes)
	case tools.KindImage:
		return "image, " + strutil.HumanSize(m.Bytes)
	default:
		if m.Lines > 0 {
			return fmt.Sprintf("%d lines, %s", m.Lines, strutil.HumanSize(m.Bytes))
		}
		return strutil.HumanSize(m.Bytes)
	}
}

// splice replaces the span of out covered by ref with repl.
func splice(out string, ref Ref, repl string) string {
	return out[:ref.Start] + repl + out[ref.End:]
}
