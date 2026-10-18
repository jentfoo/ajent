package compact

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// Options configures one compaction pass over a branch.
type Options struct {
	Cwd          string                          // canonical path base for superseded read/edit detection
	Instructions string                          // /compact <instructions>, appended to the summary prompt
	Retain       llm.RetainPolicy                // session retention policy, for measurement
	Base         int                             // fixed request overhead (system block + tool schemas), added to every measure
	Resolve      func(string) (llm.Model, error) // model-key lookup for origin stamps, always wired
	MinSteps     int                             // recent steps kept verbatim however large, 0 uses defaultMinSteps
	// VerbatimTokens caps the band past that floor. 0 uses a tenth of the compaction point.
	VerbatimTokens int
}

const (
	defaultMinSteps        = 2    // recent steps always kept verbatim, twin of defaultVerbatimDivisor
	defaultVerbatimDivisor = 10   // band ceiling divisor on the compaction point, twin of defaultMinSteps
	maxVerbatimSteps       = 8    // upper bound, matching the /settings minSteps row of 1..8
	minVerbatimTokens      = 1024 // floor, so an unknown window still keeps a modest band
	minSpanTokens          = 1024 // below this a summary cannot pay for itself
)

// Result is what one compaction produced: enough to write a session entry and rebuild context.
type Result struct {
	Summary          string
	FirstKeptEntryID string
	Before           int // estimated tokens the next request carries now
	After            int // estimated tokens it will carry once this plan is recorded
	Reduce           session.Reduce
	// Warnings name defects in what was recorded that did not fail the run.
	Warnings []string
}

// RunPrompt performs one summarisation model call and returns its assistant text.
// It is wired by the driver to a real provider stream.
type RunPrompt func(ctx context.Context, run llm.Request) (string, error)

// Compact folds everything before the verbatim band into a checkpoint. Both ends
// are measured through ContextMessages against the branch as a prior compaction
// already left it, so the reported saving is the saving the next request gets. A
// nil result means nothing worth doing changed.
func Compact(ctx context.Context, branch []session.Entry, model llm.Model, run RunPrompt, opts Options) (*Result, error) {
	minSteps, verbatimTokens := resolveVerbatim(model, opts)

	// the baseline is the effective current context, not the raw branch: a prior
	// compaction already folded part of it away. An undecodable prior is the zero
	// plan, so this pass recomputes cumulatively from the unreduced branch.
	prior, priorIdx, has, perr := session.NewestCompaction(branch)
	if has {
		// resolve a summary-only prior to an explicit FirstKeptEntryID so carrying it
		// into the new compaction entry cannot re-cut at the new entry's own position,
		// dropping everything between the two. With no following message entry,
		// summary-only semantics stay correct.
		if prior.Summary != "" && prior.FirstKeptEntryID == "" {
			if i := slices.IndexFunc(branch[priorIdx+1:], func(e session.Entry) bool { return e.Type == session.TypeMessage }); i >= 0 {
				prior.FirstKeptEntryID = branch[priorIdx+1+i].ID
			}
		}
	}
	priorCut := session.CutIndex(branch, prior)
	if priorCut < 0 { // a missing cut means the effective context is unrecoverable
		return nil, errors.New("compact: prior compaction's kept entry is missing from the branch")
	}

	view := newBranchView(branch) // one decode/token cache shared by every stage
	// resolve against caps like Prepare does, so the ceiling grades the policy
	// the wire actually applies
	view.setRetain(llm.ResolveRetain(opts.Retain, model.Caps))
	band, ok := view.chooseCut(priorCut, minSteps, verbatimTokens)
	if !ok || view.spanTokens(priorCut, band) < minSpanTokens {
		return nil, nil
	}
	var firstKept string
	if band >= 0 && band < len(branch) && branch[band].Type == session.TypeMessage {
		firstKept = branch[band].ID
	}
	if firstKept == "" { // the band opens on an assistant message, providers reject otherwise
		return nil, errors.New("compact: a cut needs a kept message entry")
	}

	before := tokensFor(branch, prior, model, opts.Retain, opts.Base, opts.Resolve)
	// the stubs never reach context: everything they touch is replaced by the
	// summary. They exist only so the summariser reads what is already known to be
	// superseded as a marker rather than as bytes.
	stubs := spanStubs(branch, priorCut, band, opts.Cwd)

	summary, nsum, err := summarise(ctx, view, priorCut, band, stubs, model, run, opts)
	if err != nil {
		return nil, err
	}

	res := &Result{
		Before: before, Summary: summary, FirstKeptEntryID: firstKept,
		// the recorded plan is inert by construction, so it carries only what the
		// notice needs, claiming stub work here would describe context that never changed
		Reduce: session.Reduce{Stats: session.Stats{Summarized: nsum}},
	}
	if perr != nil {
		// Before names the unreduced branch, larger than the last run left it
		res.Warnings = append(res.Warnings, "invalid prior compaction entry: "+perr.Error())
	}
	// a defective checkpoint still records; the warning only makes the defect visible
	if miss := missingSections(summary); len(miss) > 0 {
		res.Warnings = append(res.Warnings, "summary is missing required sections: "+strings.Join(miss, ", "))
	}
	cd := session.CompactionData{Summary: summary, FirstKeptEntryID: firstKept, Reduce: &res.Reduce}
	res.After = tokensFor(branch, cd, model, opts.Retain, opts.Base, opts.Resolve)
	if res.After < res.Before { // the plan must measurably shrink the next request
		return res, nil
	}
	return nil, nil // nothing saved, or it would grow context
}

// resolveVerbatim returns the band bounds: opts' overrides when set, else two
// steps and a tenth of the model's compaction point. Every trigger uses the same
// band. A manual run is not an instruction to keep less recent work.
func resolveVerbatim(model llm.Model, opts Options) (steps, budget int) {
	steps = min(opts.MinSteps, maxVerbatimSteps)
	if steps <= 0 {
		steps = defaultMinSteps
	}
	budget = opts.VerbatimTokens
	if budget <= 0 {
		// CompactAt is 0 on an unknown window, which would leave the band unbounded
		budget = max(minVerbatimTokens, tokens.CompactAt(model)/defaultVerbatimDivisor)
	}
	return steps, budget
}
