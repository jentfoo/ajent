package plan

import (
	"slices"

	"github.com/jentfoo/ajent/pkg/agent"
)

// persisted is the workflow state written as a latest-wins session entry and
// read back on resume.
type persisted struct {
	Phase          Phase    `json:"phase"`
	Planner        string   `json:"planner"`
	Implementor    string   `json:"implementor"`
	SavedModel     string   `json:"savedModel,omitempty"`
	SavedTools     []string `json:"savedTools,omitempty"`
	ApprovedPlan   string   `json:"approvedPlan,omitempty"`
	RevisionRounds []string `json:"revisionRounds,omitempty"`
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
		RevisionRounds: c.revisionRounds,
		GoalCaptured:   c.goalCaptured,
		PlanTip:        c.planTip,
		ReviewTip:      c.reviewTip,
	})
	if err != nil {
		c.notify("could not record workflow state ("+err.Error()+"); a resume will not "+
			"pick this up", agent.LevelWarn)
	}
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
	planner, ok := c.h.ResolveModel(p.Planner)
	if !ok {
		c.notify("plan workflow not resumed: planner "+p.Planner+" is unavailable", agent.LevelWarn)
		return false
	}
	implementor, ok := c.h.ResolveModel(p.Implementor)
	if !ok {
		c.notify("plan workflow not resumed: implementor "+p.Implementor+" is unavailable",
			agent.LevelWarn)
		return false
	}
	saved := implementor
	if p.SavedModel != "" {
		if m, found := c.h.ResolveModel(p.SavedModel); found {
			saved = m
		}
	}

	c.planner, c.implementor, c.savedModel = planner, implementor, saved
	c.savedTools = slices.Clone(p.SavedTools)
	c.approvedPlan = p.ApprovedPlan
	c.revisionRounds = slices.Clone(p.RevisionRounds)
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
	c.notify("resumed plan workflow: "+p.Phase.String(), agent.LevelInfo)
	return true
}
