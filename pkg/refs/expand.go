package refs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// idPrefix leads every injected @ reference call id, passed to the run
// sequencer at construction so the transcript format stays package-local.
const idPrefix = "ref-"

// call id kinds, one per injected tool. The path half of an id is namespaced by
// kind because a written path can imitate another kind's tag: a file named
// "ls-foo" beside a directory "foo". No kind may be a prefix of another.
const (
	readKind = "r-"
	lsKind   = "ls-"
)

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
	full  string          // resolved for the land-time tracker re-check, empty for a glob ls
	input json.RawMessage // the call's arguments, marshalled at plan time
	body  int64           // expected result bytes, for the submit reserve
}

// newInjection plans one call, marshalling its arguments now so the reserve and
// the landed pair are sized from exactly the same call.
func (x *Expander) newInjection(run int64, name, idKind, path, full string, body int64) injection {
	input, _ := json.Marshal(map[string]any{"path": path})
	return injection{name: name, id: x.seq.CallID(run, idKind+path), path: path,
		full: full, input: input, body: body}
}

// Expander turns @ references in a message into synthetic read/ls tool-call +
// result pairs, or annotates them in place when too large or non-text.
type Expander struct {
	reg     *tools.Registry
	sink    agent.Sink
	policy  tools.PathPolicy
	tracker *tools.Tracker
	seq     *agent.RunSequencer // numbers each Expand so its call ids stay unique
	// vision reports whether the active model reads images. Image references
	// are only injected when it does, nil meaning never.
	vision func() bool
}

// NewExpander returns an expander backed by reg. read/ls run through the sink so
// their display order matches the transcript order. policy resolves @ paths to
// the same keys read/write/edit use, vision reporting whether the active model
// reads images, nil meaning never.
func NewExpander(reg *tools.Registry, sink agent.Sink, policy tools.PathPolicy, vision func() bool) *Expander {
	var tracker *tools.Tracker
	if reg != nil {
		tracker = reg.Tracker()
	}
	return &Expander{reg: reg, sink: sink, policy: policy, tracker: tracker,
		seq: agent.NewRunSequencer(idPrefix), vision: vision}
}

// Seed raises the run counter above every @ reference id already in msgs. Call
// it whenever the context is rebuilt (resume, rewind, fork) so a later Expand
// never mints an id the context already holds.
func (x *Expander) Seed(msgs []llm.Message) {
	x.seq.Seed(msgs)
}

// noteEdit says how one mention's written form lands in the rewritten text.
type noteEdit uint8

const (
	noteKept      noteEdit = iota // the literal stands as written
	noteAnnotated                 // replaced with @path (measurement)
	noteStripped                  // any absorbed measurement dropped, path bare
)

// refEdit is one mention's planned text edit: its note fate plus the measured
// shape an annotation shows.
type refEdit struct {
	note  noteEdit
	shape tools.Measurement // read when note is noteAnnotated
}

// Expand plans every @ reference in text. Small text files plan a read pair
// (deduped against the tracker and against the rest of the message at its first
// mention), directories an ls pair regardless of enabled state, and large or
// non-text files annotate in place once per path however often it is named.
// Classification is existence-first: a metacharacter name that resolves is
// treated as a path, and only a written form naming nothing real expands as a
// wildcard. Re-expanding an annotated ref replaces its measurement rather than
// appending a second.
func (x *Expander) Expand(text string) Result {
	refs := Parse(text)
	if len(refs) == 0 || x.reg == nil {
		return Result{Text: text}
	}
	run := x.seq.Next()
	var (
		plan    []injection
		notices []string
		spent   int
	)
	seen := make(map[string]struct{}, len(refs))
	// one output per path however often the message names it: a repeat would
	// otherwise duplicate content, call ids or notices. keep reports whether this
	// is the first mention of key, and every emitting branch guards on it.
	keep := func(key string) bool {
		if _, dup := seen[key]; dup {
			return false
		}
		seen[key] = struct{}{}
		return true
	}
	edits := make([]refEdit, len(refs))
	// annotateFirst shows the measurement on ref i when it is this message's
	// first mention of full and reports whether that was here. A repeat stays
	// bare: its output already appears at the first mention.
	annotateFirst := func(i int, full string, m tools.Measurement) bool {
		if !keep(full) {
			edits[i] = refEdit{note: noteStripped}
			return false
		}
		edits[i] = refEdit{note: noteAnnotated, shape: m}
		return true
	}
	// classified forward in document order, so dedupe keeps first-mention
	// position and the plan lands in prose order.
	for i, ref := range refs {
		path, full, m, err := x.measureRef(ref.Path)
		if err != nil {
			// nothing real at the written form: a metacharacter name is a wildcard
			// reference, listed via ls so the model sees which paths matched before
			// choosing what to read. Patterns never resolve, dedupe as written.
			if tools.HasGlob(ref.Path) {
				if keep("ls:" + ref.Path) {
					plan = append(plan, x.lsCall(run, ref.Path, ""))
				}
				continue
			}
			// a repeat of a missing path would only repeat the warning
			if keep("miss:" + ref.Path) {
				notices = append(notices, err.Error())
			}
			continue
		}
		switch {
		case m.Dir:
			// skip a repeat listing whose output would be identical: the dir's
			// entries were observed by an earlier @ or agent ls and are unchanged.
			if x.tracker != nil && x.tracker.UnchangedDir(full) {
				continue
			}
			if keep("ls:" + full) {
				plan = append(plan, x.lsCall(run, path, full))
			}
		case m.Kind == tools.KindImage:
			// a vision model gets the picture itself through the read tool, a
			// text-only one keeping today's annotation so the reference stays visible
			if x.vision == nil || !x.vision() {
				annotateFirst(i, full, m)
				continue
			}
			edits[i].note = noteStripped // content lands behind the message or already sits in context
			if x.tracker != nil && x.tracker.Unchanged(full) {
				continue
			}
			if !keep(full) {
				continue
			}
			// images reserve their nominal size against the same total cap text reads pay,
			// so many distinct references cannot inject uncounted
			if spent+imageNominalBytes > tools.RefTotalLimit().Bytes {
				edits[i] = refEdit{note: noteAnnotated, shape: m}
				notices = append(notices, "@"+ref.Path+" "+budgetRefusal)
				continue
			}
			plan = append(plan, x.newInjection(run, "read", readKind, path, full, imageNominalBytes))
			spent += imageNominalBytes
		default:
			if m.Kind != tools.KindText {
				annotateFirst(i, full, m)
				continue
			}
			// a shape too big on its own is annotated whatever else the message holds:
			// the per-ref limit is about the file, not the budget. Lines and bytes each
			// bind only when configured.
			mtr := tools.RefInjectLimit()
			if (mtr.Lines > 0 && m.Lines > mtr.Lines) || (mtr.Bytes > 0 && m.Bytes > int64(mtr.Bytes)) {
				if annotateFirst(i, full, m) {
					notices = append(notices, "@"+ref.Path+" too large; annotated")
				}
				continue
			}
			// dedupe before the total cap: an unchanged or repeat mention reserves
			// nothing, so counting it is what falsely annotated repeats as too large
			if x.tracker != nil && x.tracker.Unchanged(full) || !keep(full) {
				edits[i].note = noteStripped // the literal stays bare
				continue
			}
			if spent+int(m.Bytes) > tools.RefTotalLimit().Bytes {
				edits[i] = refEdit{note: noteAnnotated, shape: m}
				notices = append(notices, "@"+ref.Path+" "+budgetRefusal)
				continue
			}
			// within cap: content is injected now, so an absorbed size claim from a
			// prior larger measurement must not survive
			edits[i].note = noteStripped
			// the tool returns line-numbered text, so what lands is the file plus a
			// prefix per line, not its raw size
			plan = append(plan, x.newInjection(run, "read", readKind, path, full, tools.ReadBytes(m)))
			spent += int(m.Bytes)
		}
	}
	// splices go back to front so earlier offsets stay valid; the plan is already
	// in document order from the forward pass.
	out := text
	for i := len(refs) - 1; i >= 0; i-- {
		switch edits[i].note {
		case noteAnnotated:
			out = splice(out, refs[i], annotate(refs[i], edits[i].shape))
		case noteStripped:
			out = stripNote(out, refs[i])
		}
	}
	// sized here rather than per branch: an injection that forgot to add its own
	// cost is exactly how ls came to reserve nothing at all. Each pair bills the
	// same framing EstimateMessages charges once appended, so the bar does not
	// step as the reads arrive.
	var est int
	for _, p := range plan {
		est += tokens.EstimateToolPair(
			llm.ToolCallBlock{ID: p.id, Name: p.name, Input: p.input}, p.body, tokens.KindCode)
	}
	res := Result{Text: out, Est: est, Notices: notices}
	if len(plan) > 0 {
		res.Run = func(ctx context.Context) []llm.Message {
			msgs := make([]llm.Message, 0, 2*len(plan)) // a call + result per injection
			for _, p := range plan {
				if ctx.Err() != nil {
					break // an interrupt stops the batch rather than injecting error pairs
				}
				// re-checked here, not just at plan time: another message in the same
				// batch may have read or listed it since
				if p.full != "" && x.tracker != nil {
					if p.name == "ls" {
						if x.tracker.UnchangedDir(p.full) {
							continue
						}
					} else if x.tracker.Unchanged(p.full) {
						continue
					}
				}
				// a vanished tool drops the pair silently; InjectPair's error is
				// swallowed too: a failed injection annotates rather than fails the prompt
				if tool, ok := x.reg.Lookup(p.name); ok {
					pair, _ := agent.InjectPair(ctx, tool, x.sink,
						agent.ToolCall{ID: p.id, Name: p.name, Input: p.input}, p.name+" "+p.path)
					msgs = append(msgs, pair...)
				}
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

// imageNominalBytes is what an image read reserves: a flat stand-in, not a
// measurement, since the fitted block's size is unknowable before the read.
// 6144 bytes prices the pair near imageMaxTokens at the code byte ratio.
const imageNominalBytes = 6144

// budgetRefusal completes a declined read's notice: remaining budget rather
// than file size is the reason, keeping it distinct from the "too large" one.
const budgetRefusal = "exceeds the remaining reference budget; annotated"

// measureRef resolves token to an absolute path and measures it. The returned
// path is what expansion acts on, full its absolute resolution. A written form
// naming nothing real is retried without its trailing '.' or ':' (Parse keeps
// them inside the token: dotted names and x:y need both). The trimmed reading
// wins only by existing, so ./x and dotted names resolve exactly as written.
func (x *Expander) measureRef(token string) (path, full string, m tools.Measurement, err error) {
	path = token
	if full, m, err = x.measureOne(path); err == nil {
		return path, full, m, nil
	}
	trimmed := strings.TrimRight(path, ".:")
	if trimmed == "" || trimmed == path {
		return "", "", tools.Measurement{}, err
	}
	tFull, tm, terr := x.measureOne(trimmed)
	if terr != nil {
		return "", "", tools.Measurement{}, err // the written form's failure is what to report
	}
	return trimmed, tFull, tm, nil
}

// measureOne resolves one path spelling to an absolute path and measures it.
func (x *Expander) measureOne(path string) (string, tools.Measurement, error) {
	full, err := x.policy.Resolve(path)
	if err != nil {
		return "", tools.Measurement{}, fmt.Errorf("could not resolve @%s: %w", path, err)
	}
	m, err := tools.Measure(full)
	if err != nil {
		return "", tools.Measurement{}, fmt.Errorf("@%s not found", path)
	}
	return full, m, nil
}

// lsCall plans the directory listing for a glob or directory reference. dir is
// the resolved path when it names one directory (so Run can re-check it), empty
// for a glob.
func (x *Expander) lsCall(run int64, path, dir string) injection {
	return x.newInjection(run, "ls", lsKind, path, dir, lsNominalBytes)
}

// annotate returns the replacement text for a large/non-text reference: the
// @path plus its measurement. An existing annotation on ref is replaced, never doubled.
func annotate(ref Ref, m tools.Measurement) string {
	return "@" + ref.Path + " (" + measurementText(m) + ")"
}

// stripNote drops an absorbed size claim from a mention whose output landed
// elsewhere: injected or shown at the first mention, so any stale measurement
// must not survive.
func stripNote(out string, ref Ref) string {
	if ref.Note == "" {
		return out
	}
	return splice(out, ref, "@"+ref.Path)
}

// measurementText renders the annotation shape: "800 lines, 64kb" for text,
// "binary, 1.2mb" for binary, "image, 1.2mb" for images, "special" for a pipe,
// device or socket, "dir" for a directory.
func measurementText(m tools.Measurement) string {
	if m.Dir {
		return "dir"
	}
	switch m.Kind {
	case tools.KindBinary:
		return "binary, " + strutil.HumanSize(m.Bytes)
	case tools.KindImage:
		return "image, " + strutil.HumanSize(m.Bytes)
	case tools.KindSpecial:
		return "special"
	default:
		if m.Lines > 0 {
			return fmt.Sprintf("%d lines, %s", m.Lines, strutil.HumanSize(m.Bytes))
		}
		return strutil.HumanSize(m.Bytes)
	}
}

// splice returns out with the span of ref replaced by repl.
func splice(out string, ref Ref, repl string) string {
	return out[:ref.Start] + repl + out[ref.End:]
}
