package plan

import (
	"context"
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// BeforePrompt rewrites a submitted input before it starts a turn, reporting
// whether it changed anything. It runs on the pump goroutine with the agent
// idle, so it may switch branches.
func (c *Controller) BeforePrompt(ctx context.Context, in agent.Input) (agent.Input, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	text := strings.TrimSpace(in.Text)
	switch {
	case c.phase == PhasePlanning && !in.Injected && !c.goalCaptured && text != "":
		// the contract rides as its own block so the echoed line stays the goal.
		// appendSteer emits Text before Blocks, keeping goal then contract
		c.goalCaptured = true
		c.persistLocked()
		in.Blocks = append(llm.BlockList{llm.TextBlock{Text: planningContract()}}, in.Blocks...)
		return in, true

	case c.phase == PhaseAwaitingPlan && !in.Injected && text != "":
		// whatever the user submits is the plan of record, edits included
		c.approvedPlan = text
		c.draftPlan = "" // gate passed, no draft to restore
		in.Text = c.beginImplementationLocked()
		// staged output and images would pin themselves ahead of the kickoff and its
		// planned @ reads behind it. The fresh root starts from the kickoff alone
		in.Before = nil
		in.Blocks = nil
		in.After = nil
		// the text is workflow-authored now, so the host's expansion seam skips it
		in.Prepared = true
		in.Injected = true
		return in, true

	case c.kickoffDue && (c.phase == PhaseImplementing || c.phase == PhaseReviewing):
		// a crash recorded the phase before its kickoff landed, so it rides ahead
		// of this prompt rather than the round running with nothing to go on
		c.kickoffDue = false
		kickoff := agent.MessageInfo{
			Message: llm.Text(llm.RoleUser, c.repairKickoff(ctx)), Injected: true, Replayed: true,
		}
		in.Before = append([]agent.MessageInfo{kickoff}, in.Before...)
		if c.phase == PhaseImplementing && !in.Injected {
			c.retries = 0 // hand continuation restarts the budget
		}
		return in, true

	case c.phase == PhaseImplementing && !in.Injected:
		// hand continuation restarts the budget, so a post-pause error retries
		// rather than re-pausing with no attempts left
		c.retries = 0
	}
	return in, false
}

// Advance applies the transition a control tool recorded, or the one a turn
// ending implies. It runs at every turn boundary on the drain goroutine, errored
// turns included, and returns the next turn's input when the workflow owns it.
func (c *Controller) Advance(ctx context.Context, last agent.TurnResult) (agent.Input, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.phase.active() {
		return agent.Input{}, false
	}
	// capture and clear first: an abandoned turn discards its transition
	pending := c.pending
	c.pending = nil

	if c.cancelled || last.Stop == llm.StopAborted {
		c.stopLocked("plan workflow stopped")
		return agent.Input{}, false
	}

	switch c.phase {
	case PhasePlanning:
		if pending != nil && pending.to == PhaseAwaitingPlan {
			return c.awaitPlanLocked(pending.payload), false
		}

	case PhaseImplementing:
		// a turn that errored or ran out of room stopped short of the work, so
		// reviewing it would judge an implementation that never finished
		if last.Err != nil || last.Stop == llm.StopMaxTokens {
			return c.retryImplementationLocked()
		}
		c.retries = 0
		// a stopped implementor is a finished implementor, dev_review or not. But the
		// reviewer must still hear what it did, so an unreported round falls back to
		// its closing message. Read before the fork replaces the context.
		if pending != nil && pending.to == PhaseReviewing {
			c.execSummary = pending.payload
		}
		if strings.TrimSpace(c.execSummary) == "" && c.h.Messages != nil {
			c.execSummary = llm.LastAssistantText(c.h.Messages())
		}
		return c.beginReviewLocked(ctx), true

	case PhaseReviewing:
		switch {
		case pending != nil && pending.to == PhaseDone:
			c.stopLocked("plan workflow complete")
			return agent.Input{}, false
		case pending != nil && pending.to == PhaseImplementing:
			return c.beginRevisionLocked(pending.payload)
		default:
			return c.stalledReviewerLocked(ctx)
		}
	}
	return agent.Input{}, false
}

// awaitPlanLocked parks the workflow with the plan in the editor. It starts no
// turn: the user's next submission is the gate. Caller holds mu.
func (c *Controller) awaitPlanLocked(plan string) agent.Input {
	if c.h.Head != nil {
		c.planTip = c.h.Head() // review round 1 forks from here
	}
	c.draftPlan = plan
	c.setPhaseLocked(PhaseAwaitingPlan)
	if c.h.SetInput != nil {
		c.h.SetInput(plan)
	}
	c.notify("plan ready. Edit and submit to hand it to "+c.implementor.Key()+
		", or /plan-stop to cancel", agent.LevelInfo)
	return agent.Input{}
}

// beginImplementationLocked opens a fresh root for the implementor and returns
// its kickoff text. Caller holds mu.
func (c *Controller) beginImplementationLocked() string {
	c.fork("", c.implementor) // empty context: only this round's work
	c.execSummary = ""
	c.retries = 0
	c.phase = PhaseImplementing
	c.applyScopeLocked(PhaseImplementing)
	c.setPhaseLocked(PhaseImplementing)
	c.notify("implementing with "+c.implementor.Key()+" (round "+strconv.Itoa(c.round())+
		"/"+strconv.Itoa(maxRevisions)+")", agent.LevelInfo)
	return c.implementKickoffText()
}

// beginRevisionLocked records a review round and re-enters implementation, or
// ends the workflow at the revision cap. Caller holds mu.
func (c *Controller) beginRevisionLocked(instructions string) (agent.Input, bool) {
	if c.h.Head != nil {
		c.reviewTip = c.h.Head() // later rounds continue from this review
	}
	c.revisionRounds = append(c.revisionRounds, instructions)
	if len(c.revisionRounds) >= maxRevisions {
		c.stopLocked(capReport(instructions))
		return agent.Input{}, false
	}
	return kickoffInput(c.beginImplementationLocked()), true
}

// beginReviewLocked forks onto the review branch and returns its kickoff.
// Caller holds mu.
func (c *Controller) beginReviewLocked(ctx context.Context) agent.Input {
	target := c.reviewTip
	if target == "" {
		target = c.planTip
	}
	c.fork(target, c.planner)
	c.phase = PhaseReviewing
	c.applyScopeLocked(PhaseReviewing)
	c.setPhaseLocked(PhaseReviewing)

	c.notify("reviewing with "+c.planner.Key()+" (round "+strconv.Itoa(c.round())+
		"/"+strconv.Itoa(maxRevisions)+")", agent.LevelInfo)
	return kickoffInput(c.reviewKickoffText(ctx))
}

// implementKickoffText returns the current round's implementation kickoff.
// Caller holds mu.
func (c *Controller) implementKickoffText() string {
	// the most recent revision instructions, empty on round one
	rounds := c.revisionRounds
	latest := ""
	if len(rounds) > 0 {
		latest = rounds[len(rounds)-1]
	}
	return implementKickoff(c.approvedPlan, latest)
}

// reviewKickoffText returns the review kickoff with a fresh capture of the
// working tree. Caller holds mu.
func (c *Controller) reviewKickoffText(ctx context.Context) string {
	var status, diffStat string
	if c.h.Git != nil {
		status, diffStat = c.h.Git(ctx)
	}
	return reviewKickoff(c.approvedPlan, status, diffStat, c.execSummary)
}

// repairKickoff rebuilds the kickoff a crash dropped. Caller holds mu.
func (c *Controller) repairKickoff(ctx context.Context) string {
	if c.phase == PhaseReviewing {
		return c.reviewKickoffText(ctx)
	}
	return c.implementKickoffText()
}

// retryImplementationLocked resumes an implementation turn that died on a
// provider error, or pauses the workflow once the budget is spent. Caller holds mu.
func (c *Controller) retryImplementationLocked() (agent.Input, bool) {
	c.retries++
	if c.retries > maxExecRetries {
		c.notify("implementation failed repeatedly; the workflow is paused in place. "+
			"Continue by hand or /plan-stop", agent.LevelError)
		return agent.Input{}, false
	}
	c.notify("implementation turn errored; retrying ("+strconv.Itoa(c.retries)+"/"+
		strconv.Itoa(maxExecRetries)+")", agent.LevelWarn)
	return kickoffInput(execContinue()), true
}

// stalledReviewerLocked asks the user what to do when a review ended without
// calling either control tool. Caller holds mu.
func (c *Controller) stalledReviewerLocked(ctx context.Context) (agent.Input, bool) {
	const (
		optRevise   = "Request revisions"
		optComplete = "Accept and finish"
		optContinue = "Keep reviewing"
	)
	choice, err := c.h.Ask(ctx, "The review ended without a verdict. What now?",
		[]string{optRevise, optComplete, optContinue})
	if err != nil {
		return agent.Input{}, false
	}
	switch choice {
	case 0:
		c.notify("tell the reviewer what still needs work, or /plan-stop", agent.LevelInfo)
	case 1:
		c.stopLocked("plan workflow complete")
	}
	return agent.Input{}, false
}

// kickoffInput wraps one round of workflow-authored turn text. Prepared marks it
// past the host's expansion seam: an @path quoted in a plan, summary or
// instruction is a pointer for the receiving model, never a reference to expand.
func kickoffInput(text string) agent.Input {
	return agent.Input{Text: text, Injected: true, Prepared: true}
}

// fork switches the workflow onto head with model m, reporting a failure rather
// than continuing against stale state. Caller holds mu.
func (c *Controller) fork(head string, m llm.Model) {
	if c.h.Fork == nil {
		return
	}
	if err := c.h.Fork(head, m); err != nil {
		c.notify("could not complete the branch switch ("+err.Error()+"), the context "+
			"may be stale, consider /plan-stop", agent.LevelError)
	}
}
