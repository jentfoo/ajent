package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
}

// compaction returns the live compaction settings, or the built-in defaults.
func (c *compactor) compaction() config.Compaction {
	if c.cfg == nil {
		return config.Compaction{Auto: true}
	}
	return c.cfg()
}

// run performs one compaction for reason, returning whether anything changed. A
// manual run refuses while a turn streams; a threshold run only acts when Used has
// crossed the model's compaction point; an overflow run fires mid-turn from the
// turn's own goroutine.
func (c *compactor) run(ctx context.Context, reason agent.CompactReason, instructions string) (bool, error) {
	if reason != agent.CompactOverflow && c.ag != nil && c.ag.Running() {
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
	if reason == agent.CompactThreshold {
		if !cfg.Auto {
			return false, nil // automatic reduction disabled by config
		}
		t := c.st.Tokens
		at := tokens.CompactAt(model)
		if t == nil || at <= 0 || t.Context().Used < at {
			return false, nil // not at the compaction point yet
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
	run := func(ctx context.Context, req llm.Request) (string, error) {
		text, usage, serr := runSummary(ctx, provider, req)
		if t := c.st.Tokens; t != nil && serr == nil {
			// spend-only: the summariser's prompt is not this session's context, so a
			// failed compaction must not leave the bar at its (much larger) size
			t.Spend(model.Key(), usage)
		}
		return text, serr
	}

	// measure full usage (system + AGENTS.md + tool schemas), not just messages
	var base int
	if c.ag != nil && reason != agent.CompactOverflow {
		base = c.ag.BaseEstimate(true) // 0 only mid-turn, where the reseed is transient anyway
	}
	if instructions == "" && c.focus != nil {
		instructions = c.focus() // a plan phase keeps its own focus across auto-compaction
	}
	opts := compact.Options{
		Cwd:            cwdOrDot(),
		Instructions:   instructions,
		Retain:         c.st.Reasoning.Retain,
		Base:           base,
		MinSteps:       cfg.MinSteps,
		VerbatimTokens: verbatimTokens(model, cfg.VerbatimFraction),
	}
	res, cerr := compact.Compact(ctx, branch, model, run, opts)
	if cerr != nil {
		c.notify("compaction failed: "+cerr.Error(), agent.LevelWarn)
		return false, cerr
	}
	if res == nil {
		if reason == agent.CompactManual {
			c.notify("nothing to compact", agent.LevelInfo)
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
	if c.ag != nil && reason != agent.CompactOverflow {
		c.ag.WithState(func(s *agent.State) { s.Messages = rebuilt.Messages })
	} else {
		// overflow runs on the turn goroutine, where WithState refuses
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
	return true, nil
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

// errTruncated reports a response the provider stopped at its output token cap;
// its text is partial and must never be persisted or acted on.
var errTruncated = errors.New("generation stopped at the output token cap: the response is incomplete")

// runSummary drives one summarisation model call through an accumulator and
// returns its assistant text plus the usage the provider reported. A response
// the provider stopped at its output token cap is an error, never partial text.
func runSummary(ctx context.Context, p llm.Provider, req llm.Request) (string, llm.Usage, error) {
	st, err := p.Stream(ctx, req)
	if err != nil {
		return "", llm.Usage{}, err
	}
	defer func() { _ = st.Close() }()
	stop := llm.CloseOnDone(ctx, st)
	defer stop()

	var acc llm.Accumulator
	for ev, ok := st.Next(); ok; ev, ok = st.Next() {
		acc.Add(ev)
		if ctx.Err() != nil {
			return "", llm.Usage{}, ctx.Err() // cancelled: stop consuming the response
		}
	}
	if err := ctx.Err(); err != nil {
		return "", llm.Usage{}, err // deliberate close leaves st.Err nil; never return partial text
	}
	if err := st.Err(); err != nil {
		return "", llm.Usage{}, err
	}
	if err := acc.Err(); err != nil {
		return "", llm.Usage{}, err
	}
	if acc.StopReason() == llm.StopMaxTokens {
		return "", llm.Usage{}, errTruncated
	}

	var parts []string
	for _, b := range acc.Message().Content {
		if tb, ok := b.(llm.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, ""), acc.Usage(), nil
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
