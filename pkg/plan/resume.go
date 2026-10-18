package plan

import (
	"slices"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// persisted is the workflow state written as a latest-wins session entry and read back on resume.
type persisted struct {
	Phase          Phase    `json:"phase"`
	Planner        string   `json:"planner"`
	Implementor    string   `json:"implementor"`
	SavedModel     string   `json:"savedModel,omitempty"`
	SavedTools     []string `json:"savedTools,omitempty"`
	ApprovedPlan   string   `json:"approvedPlan,omitempty"`
	DraftPlan      string   `json:"draftPlan,omitempty"`
	RevisionRounds []string `json:"revisionRounds,omitempty"`
	ExecSummary    string   `json:"execSummary,omitempty"`
	GoalCaptured   bool     `json:"goalCaptured,omitempty"`
	PlanTip        string   `json:"planTip,omitempty"`
	ReviewTip      string   `json:"reviewTip,omitempty"`
}

// persistLocked records the current state on the branch that is now live.
// Caller holds mu.
func (c *Controller) persistLocked() {
	if c.h.Persist == nil {
		return
	}
	err := c.h.Persist(persisted{
		Phase:          c.phase,
		Planner:        c.planner.Key(),
		Implementor:    c.implementor.Key(),
		SavedModel:     c.savedModel.Key(),
		SavedTools:     c.savedTools,
		ApprovedPlan:   c.approvedPlan,
		DraftPlan:      c.draftPlan,
		RevisionRounds: c.revisionRounds,
		ExecSummary:    c.execSummary,
		GoalCaptured:   c.goalCaptured,
		PlanTip:        c.planTip,
		ReviewTip:      c.reviewTip,
	})
	if err != nil {
		c.notify("could not record workflow state ("+err.Error()+"); a resume will not "+
			"pick this up", agent.LevelWarn)
	}
}

// resolveRole resolves a persisted model key, reported under its role name.
// An unresolvable key abandons the restore with a notice and reports false.
func (c *Controller) resolveRole(role, key string) (llm.Model, bool) {
	m, ok := c.h.ResolveModel(key)
	if !ok {
		c.notify("plan workflow not resumed: "+role+" "+key+" is unavailable", agent.LevelWarn)
	}
	return m, ok
}

// Restore rebuilds a mid-workflow controller from the resumed branch, reporting
// whether one was picked up. A finished workflow is never resurrected, and a
// model that no longer resolves abandons the restore with a notice.
func (c *Controller) Restore() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.h.Restore == nil || c.phase.active() {
		return false
	}

	var p persisted
	if !c.h.Restore(&p) || !p.Phase.active() {
		return false
	}
	if c.h.ResolveModel == nil {
		return false
	}
	planner, ok := c.resolveRole("planner", p.Planner)
	if !ok {
		return false
	}
	implementor, ok := c.resolveRole("implementor", p.Implementor)
	if !ok {
		return false
	}
	saved := implementor // entries predating the saved-model field have nothing of their own
	if p.SavedModel != "" {
		if saved, ok = c.resolveRole("saved model", p.SavedModel); !ok {
			return false
		}
	}

	c.planner, c.implementor, c.savedModel = planner, implementor, saved
	// a set that round-tripped empty is unknown rather than "disable everything"
	if len(p.SavedTools) > 0 {
		c.savedTools = slices.Clone(p.SavedTools)
	}
	c.approvedPlan = p.ApprovedPlan
	c.draftPlan = p.DraftPlan
	c.revisionRounds = slices.Clone(p.RevisionRounds)
	c.execSummary = p.ExecSummary
	c.goalCaptured = p.GoalCaptured
	c.planTip, c.reviewTip = p.PlanTip, p.ReviewTip
	c.retries, c.pending, c.cancelled = 0, nil, false // a resume is manual intervention

	// tools are runtime-only, so the phase scope is re-applied rather than
	// reconstructed from the branch's setting entries
	if c.h.AddTools != nil {
		c.h.AddTools(controlTools(c))
	}
	c.phase = p.Phase
	c.applyScopeLocked(p.Phase)
	c.setPhaseLocked(p.Phase)
	c.repairFromBranch()
	// a parked gate loses its draft unless it is put back, so the user can read,
	// edit and submit rather than an arbitrary prompt becoming the plan of record
	if c.phase == PhaseAwaitingPlan && c.draftPlan != "" && c.h.SetInput != nil {
		c.h.SetInput(c.draftPlan)
	}
	c.notify("resumed plan workflow: "+p.Phase.String(), agent.LevelInfo)
	return true
}

// repairFromBranch validates the restored phase against the branch: a crash
// between persisting a transition and delivering its message leaves the entry
// at the tail with nothing behind it, so the missing message is re-delivered
// ahead of the next prompt. Caller holds mu.
func (c *Controller) repairFromBranch() {
	if c.h.Landed == nil || c.h.Landed() {
		return
	}
	switch c.phase {
	case PhaseImplementing, PhaseReviewing:
		// the round would run with no plan in context at all
		c.kickoffDue = true
		c.notify("the "+c.phase.String()+" kickoff never reached the transcript; it rides "+
			"ahead of the next prompt", agent.LevelWarn)
	case PhasePlanning:
		if c.goalCaptured {
			// the goal was recorded as captured before its message landed, so the
			// next prompt becomes the goal
			c.goalCaptured = false
			c.notify("the recorded goal never reached the transcript; the next prompt "+
				"becomes the goal", agent.LevelWarn)
		}
	}
}
