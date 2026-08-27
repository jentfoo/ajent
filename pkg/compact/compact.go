// Package compact reduces a session's context before it overflows. It keeps the
// most recent steps verbatim and folds everything older into one structured
// checkpoint, recording the plan on a compaction entry; pkg/session replays that
// plan on every rebuild so what the model sees is always exactly what was measured.
package compact

import (
	"context"
	"errors"
	"fmt"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
)

// Options configures one compaction pass over a branch.
type Options struct {
	Cwd          string           // canonical path base for superseded read/edit detection
	Instructions string           // /compact <instructions>; appended to the summary prompt
	Retain       llm.RetainPolicy // session retention policy, for measurement
	Base         int              // fixed request overhead (system block + tool schemas), added to every measure
	MinSteps     int              // recent steps kept verbatim however large; 0 uses defaultMinSteps
	// VerbatimTokens caps the band past that floor; 0 uses a tenth of the
	// compaction point.
	VerbatimTokens int
}

const (
	defaultMinSteps        = 2    // recent steps always kept verbatim; twin of defaultVerbatimDivisor
	defaultVerbatimDivisor = 10   // band ceiling divisor on the compaction point; twin of defaultMinSteps
	maxVerbatimSteps       = 8    // upper bound, matching the /settings minSteps row of 1..8
	minVerbatimTokens      = 1024 // floor, so an unknown window still keeps a modest band
	minSpanTokens          = 1024 // below this a summary cannot pay for itself
)

// Result is what one compaction produced: enough to write a session entry and
// rebuild context.
type Result struct {
	Summary          string
	FirstKeptEntryID string
	Before           int // estimated tokens the next request carries now
	After            int // estimated tokens it will carry once this plan is recorded
	Reduce           session.Reduce
}

// RunPrompt performs one summarisation model call and returns its assistant text.
// It is wired by the driver to a real provider stream; nil disables compaction.
type RunPrompt func(ctx context.Context, run llm.Request) (string, error)

// Compact folds everything before the verbatim band into a checkpoint. Both ends
// are measured through ContextMessages against the branch as a prior compaction
// already left it, so the reported saving is the saving the next request gets. A
// nil result means nothing worth doing changed.
func Compact(ctx context.Context, branch []session.Entry, model llm.Model, run RunPrompt, opts Options) (*Result, error) {
	if run == nil {
		return nil, nil // no summariser wired; there is no other way to reduce
	}
	minSteps, verbatimTokens := resolveVerbatim(model, opts)

	// the baseline is the effective current context, not the raw branch: a prior
	// compaction already folded part of it away.
	prior, priorIdx, has := session.NewestCompaction(branch)
	if has {
		prior = normalisePrior(branch, prior, priorIdx)
	}
	priorCut := session.CutIndex(branch, prior)
	if priorCut < 0 { // an unlocatable prior cut degrades to the raw branch
		priorCut, prior = 0, session.CompactionData{}
	}

	band, ok := chooseCut(branch, priorCut, minSteps, verbatimTokens)
	if !ok || spanTokens(branch, priorCut, band) < minSpanTokens {
		return nil, nil
	}
	firstKept := firstKeptID(branch, band)
	if firstKept == "" { // the band opens on an assistant message; providers reject otherwise
		return nil, errors.New("compact: a cut needs a kept message entry")
	}

	before := tokensFor(branch, prior, model, opts.Retain, opts.Base)
	// the stubs never reach context: everything they touch is replaced by the
	// summary. They exist only so the summariser reads what is already known to be
	// superseded as a marker rather than as bytes.
	stubs := spanStubs(branch, priorCut, band, opts.Cwd)

	summary, nsum, err := summarise(ctx, branch, priorCut, band, stubs, model, run, opts)
	if err != nil {
		return nil, fmt.Errorf("compact: summarise: %w", err)
	}

	res := &Result{
		Before: before, Summary: summary, FirstKeptEntryID: firstKept,
		// the recorded plan is inert by construction, so it carries only what the
		// notice needs; claiming stub work here would describe context that never changed
		Reduce: session.Reduce{Stats: session.Stats{Summarized: nsum}},
	}
	cd := session.CompactionData{Summary: summary, FirstKeptEntryID: firstKept, Reduce: &res.Reduce}
	res.After = tokensFor(branch, cd, model, opts.Retain, opts.Base)
	return finish(res)
}

// normalisePrior resolves a summary-only prior compaction to an explicit
// FirstKeptEntryID so carrying it into a new compaction entry cannot re-cut at the
// new entry's own position, dropping everything between the two. It returns cd
// unchanged when no message entry follows the compaction, where summary-only
// semantics stay correct.
func normalisePrior(branch []session.Entry, cd session.CompactionData, idx int) session.CompactionData {
	if cd.Summary == "" || cd.FirstKeptEntryID != "" {
		return cd
	}
	for i := idx + 1; i < len(branch); i++ {
		if branch[i].Type == session.TypeMessage {
			cd.FirstKeptEntryID = branch[i].ID
			return cd
		}
	}
	return cd
}

// firstKeptID returns the entry id the band starts at.
func firstKeptID(branch []session.Entry, band int) string {
	if band < 0 || band >= len(branch) || branch[band].Type != session.TypeMessage {
		return ""
	}
	return branch[band].ID
}

// finish returns res only when the plan measurably shrinks the next request.
func finish(res *Result) (*Result, error) {
	if res.After < res.Before {
		return res, nil
	}
	return nil, nil // nothing saved, or it would grow context
}

// resolveVerbatim returns the band bounds: opts' overrides when set, else two
// steps and a tenth of the model's compaction point. Every trigger uses the same
// band; a manual run is not an instruction to keep less recent work.
func resolveVerbatim(model llm.Model, opts Options) (steps, tokens int) {
	steps = min(opts.MinSteps, maxVerbatimSteps)
	if steps <= 0 {
		steps = defaultMinSteps
	}
	tokens = opts.VerbatimTokens
	if tokens <= 0 {
		// CompactAt is 0 on an unknown window, which would leave the band unbounded
		tokens = max(minVerbatimTokens, compactAt(model)/defaultVerbatimDivisor)
	}
	return steps, tokens
}
