// Package compact reduces a session's context before it overflows: cheap
// structural reductions first (stages 1-3), then an LLM summary only when they
// are not enough (stage 4). It computes a plan recorded on the compaction entry;
// pkg/session replays that plan on every rebuild so what the model sees is always
// exactly what was measured.
package compact

import (
	"context"
	"fmt"
	"slices"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
)

// Reason says why a compaction ran. It does not change the plan (every trigger
// targets half the window) but lets callers phrase their notice differently.
type Reason uint8

const (
	Manual    Reason = iota // /compact [instructions]
	Threshold               // automatic at a turn boundary when Used crosses compactAt
	Overflow                // defensive retry after llm.ErrContextOverflow
)

// Options configures one compaction pass over a branch.
type Options struct {
	Cwd          string           // canonical path base for superseded read/edit detection
	Instructions string           // /compact <instructions>; appended to the summary prompt
	TargetTokens int              // desired Used after compacting; 0 uses half the window budget
	Retain       llm.RetainPolicy // session retention policy, for stage 3
	Caps         llm.Capabilities // model caps, used to resolve that retention policy
	Force        bool             // always summarise older turns even far below target (manual /compact)
}

// Result is what one compaction produced: enough to write a session entry and
// rebuild context. FirstKeptEntryID empty with a Summary set means a
// summary-only compaction: everything before the compaction entry is folded.
type Result struct {
	Summary          string
	FirstKeptEntryID string
	Before           int // estimated tokens before, via ContextMessages with no plan
	After            int // estimated tokens after applying the whole plan + summary
	Reduce           session.Reduce
}

// RunPrompt performs one summarisation model call and returns its assistant text.
// It is wired by the driver to a real provider stream; nil disables stage 4.
type RunPrompt func(ctx context.Context, req llm.Request) (string, error)

// Compact reduces branch toward opts.TargetTokens. Stages run in order and each
// reduction is measured through ContextMessages so an early exit reports exactly
// what the next request would send. A nil result means nothing worth doing changed.
func Compact(ctx context.Context, branch []session.Entry, model llm.Model, run RunPrompt, opts Options) (*Result, error) {
	target := resolveTarget(model, opts.TargetTokens)
	before := tokensFor(branch, session.CompactionData{})

	r := &session.Reduce{}
	var stats session.Stats

	// stages 1-3 are free; run them all (even when stage 4 will be needed) so the
	// summary has less to read.
	if stubs, drops, s := structural(branch, opts.Cwd); len(stubs)+len(drops) > 0 {
		r.Stubs = append(r.Stubs, stubs...)
		r.Drop = append(r.Drop, drops...)
		stats.Failed += s.Failed
		stats.Superseded += s.Superseded
		stats.Aborted += s.Aborted
	}
	if elided := truncate(branch, opts.Cwd, r); len(elided) > 0 {
		r.Stubs = append(r.Stubs, elided...)
		stats.Truncated += len(elided)
	}
	if resolveRetain(opts.Retain, opts.Caps) == llm.RetainAll {
		r.StripThinking = true
	}
	r.Stats = stats // record what the stages did, for the notice

	res := &Result{Before: before, Reduce: *r}
	after123 := tokensFor(branch, session.CompactionData{Reduce: r})
	// forced proceeds even under target so older turns can fold into a summary
	if !opts.Force && (after123 <= target || len(branch) == 0) {
		res.After = after123
		return finish(res)
	}

	if run == nil {
		// no summariser wired; report whatever stages 1-3 saved.
		res.After = after123
		return finish(res)
	}

	summary, firstKept, nsum, err := stage4(ctx, branch, model, run, opts)
	if err != nil {
		return nil, fmt.Errorf("compact: summarise: %w", err)
	}
	r.Stats.Summarized = nsum
	res.Reduce = *r
	res.Summary = summary
	res.FirstKeptEntryID = firstKept

	cd := session.CompactionData{Summary: summary, FirstKeptEntryID: firstKept, Reduce: r}
	res.After = tokensFor(measureBranch(branch, cd), cd)
	return finish(res)
}

// measureBranch returns branch as it will look once cd is recorded: a
// summary-only plan cuts at the compaction entry itself, which does not exist yet.
func measureBranch(branch []session.Entry, cd session.CompactionData) []session.Entry {
	if cd.Summary == "" || cd.FirstKeptEntryID != "" {
		return branch
	}
	return append(slices.Clone(branch), session.Entry{Type: session.TypeCompaction})
}

// finish returns res only when the plan measurably shrinks the next request.
func finish(res *Result) (*Result, error) {
	if res.After < res.Before {
		return res, nil
	}
	return nil, nil // nothing saved, or it would grow context
}

// resolveTarget returns the after-compaction token target: opts' override when set,
// else half of the usable window budget.
func resolveTarget(model llm.Model, override int) int {
	if override > 0 {
		return override
	}
	budget := model.ContextWindow - tokensReserve(model)
	if budget <= 1 {
		return 1024 // unknown or tiny window: fall back to a modest fixed target
	}
	return max(2, budget/2)
}

// resolveRetain applies the caps adjustment to a retention policy.
func resolveRetain(p llm.RetainPolicy, c llm.Capabilities) llm.RetainPolicy {
	return llm.ResolveRetain(p, c)
}
