// Package plan runs a two-model development workflow: a planner drafts, the
// user approves, an implementor builds against that plan alone, and the planner
// reviews the result. Each phase is a branch of the session tree rather than a
// projection of one message list, so what a model sees is what the transcript
// holds. Everything the workflow cannot import arrives through Host.
package plan

import (
	"context"
	"strconv"
	"sync"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

const (
	// Source labels the control tools in the tool registry.
	Source = "plan"
	// CustomType names the session entry carrying workflow state across a resume.
	CustomType = "plan"

	maxRevisions   = 4
	maxExecRetries = 3
)

// Phase is where a workflow currently sits.
type Phase uint8

const (
	PhaseIdle Phase = iota
	PhasePlanning
	PhaseAwaitingPlan // the plan is in the editor; the user's next submit starts work
	PhaseImplementing
	PhaseReviewing
	PhaseDone
)

// String returns the phase name used in status text and messages.
func (p Phase) String() string {
	switch p {
	case PhasePlanning:
		return "planning"
	case PhaseAwaitingPlan:
		return "awaiting plan"
	case PhaseImplementing:
		return "implementing"
	case PhaseReviewing:
		return "reviewing"
	case PhaseDone:
		return "done"
	default:
		return "idle"
	}
}

// active reports whether a workflow is running in this phase.
func (p Phase) active() bool { return p != PhaseIdle && p != PhaseDone }

// Host is the driver-supplied surface a workflow drives. main.go wires each
// field; a nil field disables that capability rather than panicking.
type Host struct {
	PickModel   func(ctx context.Context, title string) (llm.Model, bool)
	ActiveModel func() llm.Model
	Running     func() bool // a turn is in flight
	Abort       func()      // interrupt the running turn

	ToolNames    func() []string    // currently enabled names
	PlannerTools func() []string    // read-only set the planner and reviewer get
	SetTools     func([]string)     // narrow the enabled set to exactly these
	AddTools     func([]agent.Tool) // lazy dev_* registration
	DropTools    func()             // unregister them again

	// Fork points the session and agent state at head and applies m as the
	// branch's model. An empty head starts a new root.
	Fork func(head string, m llm.Model) error
	Head func() string

	Persist      func(v any) error
	Restore      func(v any) bool
	ResolveModel func(key string) (llm.Model, bool)

	// LastText is the trailing assistant text of the live context, used as the
	// implementation summary when the implementor stopped without reporting one.
	LastText func() string

	SetInput func(string)
	Ask      func(ctx context.Context, q string, opts []string) (int, error)
	Notify   func(msg string, level agent.Level)
	Status   func(text, short string) // "" clears the segment
	Git      func(ctx context.Context) (status, diffStat string)
}

// transition is a phase change a control tool recorded, applied once at the
// next turn boundary.
type transition struct {
	to      Phase
	payload string // plan, implementation summary or revision instructions
}

// Controller owns one workflow. Start and BeforePrompt run on the pump
// goroutine, Advance on the drain goroutine and control tools on tool
// goroutines, so every field sits under mu.
type Controller struct {
	mu sync.Mutex
	h  Host

	phase       Phase
	planner     llm.Model
	implementor llm.Model

	savedModel llm.Model
	savedTools []string

	approvedPlan   string
	revisionRounds []string
	execSummary    string
	goalCaptured   bool

	planTip   string // hand-off point; review round 1 forks here
	reviewTip string // tip of the review branch, for later rounds

	retries   int
	pending   *transition
	cancelled bool
}

// New returns a controller bound to h. Nothing is registered or changed until
// Start runs.
func New(h Host) *Controller { return &Controller{h: h} }

// Active reports whether a workflow is running.
func (c *Controller) Active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.phase.active()
}

// Status returns a one-line description of the workflow, empty when idle.
func (c *Controller) Status() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.phase.active() {
		return ""
	}
	return "phase=" + c.phase.String() +
		" round=" + strconv.Itoa(c.round()) + "/" + strconv.Itoa(maxRevisions) +
		" planner=" + c.planner.Key() + " implementor=" + c.implementor.Key()
}

// Start opens the planner picker and enters the planning phase, prefilling the
// editor with prefill. It reports an error message for the caller to show, or
// empty on success.
func (c *Controller) Start(ctx context.Context, prefill string) string {
	c.mu.Lock()
	if c.phase.active() {
		c.mu.Unlock()
		return "a plan workflow is already running; /plan-stop first"
	}
	if c.h.Running != nil && c.h.Running() {
		c.mu.Unlock()
		return "a turn is running; press Esc first"
	}
	if c.h.PickModel == nil || c.h.ActiveModel == nil {
		c.mu.Unlock()
		return "/plan needs an interactive terminal"
	}
	implementor := c.h.ActiveModel()
	if implementor.ID == "" {
		c.mu.Unlock()
		return "no active model to implement with; use /model first"
	}
	c.mu.Unlock()

	// the picker blocks, so it must not hold the lock
	planner, ok := c.h.PickModel(ctx, "Planner model")
	if !ok {
		return "cancelled"
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.phase.active() { // a second /plan raced this picker
		return "a plan workflow is already running; /plan-stop first"
	}
	c.planner, c.implementor = planner, implementor
	c.savedModel = implementor
	if c.h.ToolNames != nil {
		c.savedTools = c.h.ToolNames()
	}
	c.approvedPlan, c.revisionRounds, c.execSummary = "", nil, ""
	c.planTip, c.reviewTip = "", ""
	c.retries, c.pending, c.cancelled = 0, nil, false
	c.goalCaptured = false

	if c.h.AddTools != nil {
		c.h.AddTools(controlTools(c))
	}
	c.phase = PhasePlanning
	// planning continues on the current branch, so the fork is onto its own head:
	// it records the model change without moving anywhere.
	if c.h.Head != nil {
		c.fork(c.h.Head(), planner)
	}
	c.applyScopeLocked(PhasePlanning)
	c.setPhaseLocked(PhasePlanning)
	if c.h.SetInput != nil && prefill != "" {
		c.h.SetInput(prefill)
	}
	c.notify("planning with "+planner.Key()+"; describe the goal and submit", agent.LevelInfo)
	return ""
}

// Stop cancels the workflow and restores the model and tool set found at Start.
// A running turn is aborted first; its boundary completes the restore.
func (c *Controller) Stop() {
	c.mu.Lock()
	if !c.phase.active() {
		c.mu.Unlock()
		c.notify("no plan workflow is running", agent.LevelInfo)
		return
	}
	running := c.h.Running != nil && c.h.Running()
	if running {
		c.cancelled = true
		c.mu.Unlock()
		if c.h.Abort != nil {
			c.h.Abort()
		}
		return
	}
	c.stopLocked("plan workflow stopped")
	c.mu.Unlock()
}

// Focus returns the compaction focus for the active phase, empty when idle so
// an ordinary session keeps the unguided summary.
func (c *Controller) Focus() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.phase {
	case PhaseImplementing:
		return implementFocus
	case PhaseReviewing:
		return reviewFocus
	default:
		return ""
	}
}

// round is the 1-based revision round. Caller holds mu.
func (c *Controller) round() int { return len(c.revisionRounds) + 1 }

// stopLocked restores the saved scope and marks the workflow done. Caller holds mu.
func (c *Controller) stopLocked(msg string) {
	if !c.phase.active() {
		return
	}
	// land on the newest review branch when there is one, else the plan tip
	target := c.reviewTip
	if target == "" {
		target = c.planTip
	}
	if target == "" && c.h.Head != nil {
		target = c.h.Head()
	}
	if target != "" {
		c.fork(target, c.savedModel)
	}
	if c.h.SetTools != nil && c.savedTools != nil {
		c.h.SetTools(c.savedTools)
	}
	if c.h.DropTools != nil {
		c.h.DropTools()
	}
	c.pending, c.cancelled = nil, false
	c.setPhaseLocked(PhaseDone)
	c.notify(msg, agent.LevelInfo)
}

// setPhaseLocked records the phase, persists it and republishes the status
// segment. Caller holds mu.
func (c *Controller) setPhaseLocked(p Phase) {
	c.phase = p
	c.persistLocked()
	if c.h.Status == nil {
		return
	}
	if !p.active() {
		c.h.Status("", "")
		return
	}
	round := strconv.Itoa(c.round()) + "/" + strconv.Itoa(maxRevisions)
	c.h.Status("plan: "+p.String()+" (r"+round+")", "plan r"+round)
}

// applyScopeLocked switches the model and narrows the tool set for p. Caller holds mu.
func (c *Controller) applyScopeLocked(p Phase) {
	if c.h.SetTools != nil {
		c.h.SetTools(c.toolsFor(p))
	}
}

// notify reports msg through the host, if it wired one.
func (c *Controller) notify(msg string, level agent.Level) {
	if c.h.Notify != nil {
		c.h.Notify(msg, level)
	}
}
