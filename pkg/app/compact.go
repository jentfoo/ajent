package app

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/compact"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// compactor runs compaction over the live session: it reads the current branch,
// asks pkg/compact to cut-and-summarise, persists the compaction entry, rebuilds
// state and reseeds the ledger, then reports what changed.
type compactor struct {
	rec  *sessRec
	st   *agent.State
	ag   *agent.Agent
	reg  *llm.Registry
	sink agent.Sink
	// notify and busy are the front end's seams: headless writes notices to
	// stderr and has no working glyph to light.
	notify      func(msg string, level agent.Level)
	busy        func() func()
	providerFor func(llm.Model) (llm.Provider, error)
	// focus supplies a caller's summariser instructions for automatic runs; an
	// explicit /compact <instructions> still wins. nil leaves runs unguided.
	focus func() string
	// cfg supplies live compaction settings so a /settings edit takes effect on the
	// next run; nil means the built-in defaults with automatic reduction on.
	cfg func() config.Compaction
	// autoDisabled latches the automatic triggers off once a real fold attempt could
	// not reduce; only a summariser call or a hard failure sets it, never "nothing
	// worth folding yet". Atomic because automatic runs land on the turn goroutine
	// and /compact on the console's.
	autoDisabled atomic.Bool
	// stalled holds the step trigger for the rest of a turn whose compaction
	// succeeded without clearing the point; the turn boundary re-arms it.
	stalled atomic.Bool
	warned  atomic.Bool // one reminder per turn, reset at the turn boundary
}

// autoReason reports whether r is an automatic trigger, sharing the config gate,
// the compaction point and the decline latch.
func autoReason(r agent.CompactReason) bool {
	return r == agent.CompactThreshold || r == agent.CompactStep
}

// used returns the ledger's current context occupancy, or 0 when there is none.
func (c *compactor) used() int {
	if t := c.st.Tokens; t != nil {
		return t.Context().Used
	}
	return 0
}

// overPoint reports whether context has crossed m's compaction point.
func (c *compactor) overPoint(m llm.Model) bool {
	at := tokens.CompactAt(m)
	return at > 0 && c.used() >= at
}

// resumeAuto re-arms the automatic triggers, for a model switch or a context-tree jump.
func (c *compactor) resumeAuto() {
	c.autoDisabled.Store(false)
	c.stalled.Store(false)
	c.warned.Store(false)
}

// compaction returns the live compaction settings, or the built-in defaults.
func (c *compactor) compaction() config.Compaction {
	if c.cfg == nil {
		return config.Compaction{Auto: true}
	}
	return c.cfg()
}

// run performs one compaction for reason, returning whether anything changed. A
// manual run refuses while a turn streams; an automatic run only acts when Used has
// crossed the model's compaction point; step and overflow runs fire mid-turn from
// the turn's own goroutine.
func (c *compactor) run(ctx context.Context, reason agent.CompactReason, instructions string) (bool, error) {
	if !reason.MidTurn() && c.ag != nil && c.ag.Running() {
		c.notify("compaction refused: press Esc to stop the turn first", agent.LevelWarn)
		return false, nil
	}
	// an in-flight turn already has the working glyph lit
	if c.busy != nil && (c.ag == nil || !c.ag.Running()) {
		done := c.busy()
		defer done()
	}
	model := c.st.Model
	cfg := c.compaction()
	if autoReason(reason) {
		if !cfg.Auto {
			return false, nil // automatic reduction disabled by config
		}
		if reason == agent.CompactThreshold { // a real turn boundary re-arms both
			c.stalled.Store(false)
			c.warned.Store(false)
		}
		if !c.overPoint(model) {
			return false, nil // not at the compaction point yet
		}
		if c.stalled.Load() {
			return false, nil // this turn already cut as far as it can; retry at its boundary
		}
		if c.autoDisabled.Load() {
			if !c.warned.Swap(true) {
				c.notify("over the compaction point and automatic compaction cannot reduce "+
					"this session; run /compact, switch models, or rewind", agent.LevelWarn)
			}
			return false, nil
		}
	}

	path := c.rec.w.Path()
	entries, _, err := session.Read(path)
	if err != nil || len(entries) == 0 {
		return false, nil
	}
	// plan against the live head, not the file tail; they differ after a rewind
	branch := session.Branch(entries, c.rec.w.Head())

	provider, perr := c.providerFor(model)
	if perr != nil {
		c.notify(fmt.Sprintf("compaction unavailable: no summariser provider for %s (%v)", model.Key(), perr), agent.LevelWarn)
		return false, perr
	}
	// a decline is only evidence this session cannot reduce once a fold was really
	// attempted; declining before this ran means there is nothing worth folding yet
	var attempted bool
	run := func(ctx context.Context, req llm.Request) (string, error) {
		attempted = true
		// the summariser call is the slow part, and a step run stalls a turn the user
		// is watching; a free decline never reaches here, so this cannot cry wolf
		c.notify("compacting "+strutil.FormatTokens(c.used())+"…", agent.LevelInfo)
		text, usage, serr := llm.RunSummary(ctx, provider, req)
		if t := c.st.Tokens; t != nil && serr == nil {
			// spend-only: the summariser's prompt is not this session's context, so a
			// failed compaction must not leave the bar at its (much larger) size
			t.Spend(model.Key(), usage)
		}
		return text, serr
	}

	// measure full usage (system + AGENTS.md + tool schemas), not just messages
	var base int
	if c.ag != nil {
		base = c.ag.BaseEstimate(true)
	}
	if base == 0 {
		if t := c.st.Tokens; t != nil {
			base = t.Base() // mid-turn BaseEstimate reports 0; the ledger holds the real value
		}
	}
	if instructions == "" && c.focus != nil {
		instructions = c.focus() // a plan phase keeps its own focus across auto-compaction
	}
	opts := compact.Options{
		Cwd:            config.Cwd(),
		Instructions:   instructions,
		Retain:         c.st.Reasoning.Retain,
		Base:           base,
		MinSteps:       cfg.MinSteps,
		VerbatimTokens: verbatimTokens(model, cfg.VerbatimFraction),
	}
	res, cerr := compact.Compact(ctx, branch, model, run, opts)
	if cerr != nil {
		if ctx.Err() == nil { // an interrupt is not a failure of this session to reduce
			c.notify("compaction failed: "+cerr.Error(), agent.LevelWarn)
			c.declineAuto(reason)
		}
		return false, cerr
	}
	if res == nil {
		if reason == agent.CompactManual {
			c.notify("nothing to compact", agent.LevelInfo)
		}
		if attempted { // a plan that did not pay for itself, not a thin session
			c.declineAuto(reason)
		}
		return false, nil
	}

	cd := session.CompactionData{
		Summary:          res.Summary,
		FirstKeptEntryID: res.FirstKeptEntryID,
		Before:           res.Before,
		After:            res.After,
		Reduce:           &res.Reduce,
	}
	if _, aerr := c.rec.w.Append(session.TypeCompaction, cd); aerr != nil {
		return false, aerr
	}

	// rebuild from the persisted transcript and swap the context into the live
	// state so every holder sees the reduced messages. A failed re-read must not
	// empty the live agent; the persisted entry applies on the next rebuild.
	entries2, _, rerr := session.Read(path)
	if rerr != nil {
		c.notify("compaction recorded but the state rebuild failed", agent.LevelWarn)
		return false, rerr
	}
	rebuilt, warns := session.State(session.Branch(entries2, session.Head(entries2)), c.reg.Resolve)
	for _, wmsg := range warns {
		c.notify("compact: "+wmsg, agent.LevelWarn)
	}
	if c.ag != nil && !reason.MidTurn() {
		c.ag.WithState(func(s *agent.State) { s.Messages = rebuilt.Messages })
	} else {
		// step and overflow runs are on the turn goroutine, where WithState refuses
		c.st.Messages = rebuilt.Messages
	}
	// a cut or an elided result takes file content out of context that read
	// tracking still vouches for, so this rebuild reports like any other
	if c.rec != nil && c.rec.onSwitch != nil {
		c.rec.onSwitch(rebuilt.Messages)
	}

	if t := c.st.Tokens; t != nil {
		// the reseed stays an estimate: pending carries the reduced messages only
		// (After already counts base, so it is subtracted back) and the ledger's own
		// base rides on top exactly once. The calibrator's factor still applies and
		// the bar keeps its ~ marker, unlike Rebase, which is reserved for exact
		// tokenizer counts.
		t.Reseed(max(0, res.After-base))
		c.sink.Context(t.Context())
	}
	c.sink.Notice(reportLine(res), agent.LevelInfo)
	c.resumeAuto() // this session still reduces
	if autoReason(reason) && c.overPoint(model) {
		// the best cut available did not clear the point, so the next step would fold
		// one more step for another whole summariser call; wait for the turn boundary
		c.stalled.Store(true)
	}
	return true, nil
}

// declineAuto latches the automatic triggers off when an attempted fold could not
// reduce, telling the user to compact themselves. A manual run reports its own.
func (c *compactor) declineAuto(reason agent.CompactReason) {
	if !autoReason(reason) || c.autoDisabled.Swap(true) {
		return
	}
	c.warned.Store(true) // this notice is the turn's reminder
	c.notify("automatic compaction could not reduce this session; run /compact with "+
		"instructions, switch models, or rewind", agent.LevelWarn)
}

// verbatimTokens converts a configured fraction into a token ceiling against the
// model's compaction point. A fraction outside (0,1) returns 0, leaving the bound
// to pkg/compact's own default.
func verbatimTokens(m llm.Model, fraction float64) int {
	if fraction <= 0 || fraction >= 1 {
		return 0
	}
	return int(float64(tokens.CompactAt(m)) * fraction)
}

// reportLine describes a compaction honestly: before/after tokens plus how much
// history was folded. It names nothing else, because nothing else changed — the
// summariser reads a reduced transcript, but that reduction never reaches context.
func reportLine(res *compact.Result) string {
	detail := ""
	if n := res.Reduce.Stats.Summarized; n > 0 {
		detail = fmt.Sprintf(" (summarised %d messages)", n)
	}
	return fmt.Sprintf("compacted %s → %s%s",
		strutil.FormatTokens(res.Before), strutil.FormatTokens(res.After), detail)
}
