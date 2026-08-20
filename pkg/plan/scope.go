package plan

import (
	"slices"

	"github.com/go-analyze/bulk"
)

// readOnlyBase is what the planner and reviewer investigate with. Neither is
// given write or edit; bash stays gated by the permission barrier as everywhere
// else.
var readOnlyBase = []string{"read", "grep", "find", "ls", "bash"}

// implementorBase stands in when nothing was captured at /plan, so a workflow
// started without a readable tool set still hands the implementor a usable one
// rather than dev_review alone.
var implementorBase = []string{"read", "write", "edit", "bash"}

// controlNames is every tool this package registers, plus the built-in it
// enables, so an implementor scope can filter them back out.
var controlNames = []string{
	DevImplementTool, DevReviewTool, DevReviseTool, DevCompleteTool, AskUserTool,
}

// toolsFor returns the enabled tool names for p. The implementor keeps the set
// the user had at /plan plus the one tool that signals completion; the planner
// and reviewer get read-only investigation and their own control tools. Caller
// holds mu.
func (c *Controller) toolsFor(p Phase) []string {
	if p == PhaseImplementing {
		// the user's own working set, minus anything this workflow owns
		own := bulk.SliceToSet(controlNames)
		out := bulk.SliceFilter(func(n string) bool {
			_, owned := own[n]
			return !owned
		}, slices.Clone(c.savedTools))
		if len(out) == 0 {
			out = slices.Clone(implementorBase)
		}
		return append(out, DevReviewTool)
	}

	out := slices.Clone(readOnlyBase)
	if c.h.PlannerTools != nil {
		for _, n := range c.h.PlannerTools() {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	out = append(out, AskUserTool)
	if p == PhaseReviewing {
		return append(out, DevReviseTool, DevCompleteTool)
	}
	return append(out, DevImplementTool)
}
