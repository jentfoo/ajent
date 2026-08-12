package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/compact"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tui"
)

// compactor runs staged compaction over the live session. It reads the current
// branch, asks pkg/compact for a reduction plan, persists the compaction entry,
// rebuilds state and reseeds the ledger, then reports what changed.
type compactor struct {
	rec         *sessRec
	st          *agent.State
	ag          *agent.Agent
	reg         *llm.Registry
	ui          *tui.UI
	sink        agent.Sink
	providerFor func(llm.Model) (llm.Provider, error)
}

// run performs one compaction for reason, returning whether anything changed. A
// manual run refuses while a turn streams; a threshold run only acts when Used has
// crossed the model's compaction point; an overflow run fires mid-turn from the
// turn's own goroutine.
func (c *compactor) run(ctx context.Context, reason agent.CompactReason, instructions string) (bool, error) {
	if reason != agent.CompactOverflow && c.ag != nil && c.ag.Running() {
		c.ui.Notify("compaction refused: press Esc to stop the turn first", tui.LevelWarn)
		return false, nil
	}
	// an in-flight turn already has the working glyph lit
	if c.ag == nil || !c.ag.Running() {
		done := c.ui.Busy()
		defer done()
	}
	model := c.st.Model
	if reason == agent.CompactThreshold {
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

	var run compact.RunPrompt
	if provider, perr := c.providerFor(model); perr == nil {
		run = func(ctx context.Context, req llm.Request) (string, error) {
			text, usage, serr := runSummary(ctx, provider, req)
			if t := c.st.Tokens; t != nil && serr == nil {
				t.Response(model.Key(), usage, tokens.EstimateRequest(req))
			}
			return text, serr
		}
	}

	// A manual /compact is an explicit request to reduce context now, so it forces a
	// summary+cut even when the session sits far below the automatic threshold.
	force := reason == agent.CompactManual
	opts := compact.Options{
		Cwd:          cwdOrDot(),
		Instructions: instructions,
		Retain:       c.st.Reasoning.Retain,
		Caps:         model.Caps,
		Force:        force,
	}
	res, cerr := compact.Compact(ctx, branch, model, run, opts)
	if cerr != nil {
		c.ui.Notify("compaction failed: "+cerr.Error(), tui.LevelWarn)
		return false, cerr
	}
	if res == nil {
		if reason == agent.CompactManual {
			c.ui.Notify("nothing to compact", tui.LevelInfo)
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
	// state so every holder sees the reduced messages.
	entries2, _, _ := session.Read(path)
	rebuilt, warns := session.State(session.Branch(entries2, session.Head(entries2)), c.reg.Resolve)
	for _, wmsg := range warns {
		c.ui.Notify("compact: "+wmsg, tui.LevelWarn)
	}
	if c.ag != nil && reason != agent.CompactOverflow {
		c.ag.WithState(func(s *agent.State) { s.Messages = rebuilt.Messages })
	} else {
		// overflow runs on the turn goroutine, where WithState refuses
		c.st.Messages = rebuilt.Messages
	}

	if t := c.st.Tokens; t != nil {
		t.Reseed(res.After)
		c.sink.Context(t.Context())
	}
	c.sink.Notice(reportLine(res), agent.LevelInfo)
	return true, nil
}

// runSummary drives one summarisation model call through an accumulator and
// returns its assistant text plus the usage the provider reported.
func runSummary(ctx context.Context, p llm.Provider, req llm.Request) (string, llm.Usage, error) {
	st, err := p.Stream(ctx, req)
	if err != nil {
		return "", llm.Usage{}, err
	}
	defer func() { _ = st.Close() }()

	var acc llm.Accumulator
	for ev, ok := st.Next(); ok; ev, ok = st.Next() {
		acc.Add(ev)
	}
	if err := st.Err(); err != nil {
		return "", llm.Usage{}, err
	}
	if err := acc.Err(); err != nil {
		return "", llm.Usage{}, err
	}

	var parts []string
	for _, b := range acc.Message().Content {
		if tb, ok := b.(llm.TextBlock); ok {
			parts = append(parts, tb.Text)
		}
	}
	return strings.Join(parts, ""), acc.Usage(), nil
}

// reportLine describes a compaction honestly: before/after tokens plus one clause
// per stage that did something, so users know what was dropped and why.
func reportLine(res *compact.Result) string {
	s := res.Reduce.Stats
	var clauses []string
	if s.Failed > 0 {
		clauses = append(clauses, fmt.Sprintf("dropped %d failed tool results", s.Failed))
	}
	if s.Superseded > 0 {
		clauses = append(clauses, fmt.Sprintf("stubbed %d superseded reads/edits", s.Superseded))
	}
	if s.Truncated > 0 {
		clauses = append(clauses, fmt.Sprintf("truncated %d outputs", s.Truncated))
	}
	if s.Aborted > 0 {
		clauses = append(clauses, fmt.Sprintf("dropped %d aborted messages", s.Aborted))
	}
	if res.Summary != "" {
		clauses = append(clauses, "summarised")
	}
	detail := ""
	if len(clauses) > 0 {
		detail = " (" + strings.Join(clauses, ", ") + ")"
	}
	return fmt.Sprintf("compacted %s → %s%s",
		tui.FormatTokens(res.Before), tui.FormatTokens(res.After), detail)
}
