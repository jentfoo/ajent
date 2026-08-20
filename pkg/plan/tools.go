package plan

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// Control tool names. AskUserTool is a built-in the planning and reviewing
// scopes enable rather than one this package registers.
const (
	DevImplementTool = "dev_implement"
	DevReviewTool    = "dev_review"
	DevReviseTool    = "dev_revise"
	DevCompleteTool  = "dev_complete"
	AskUserTool      = "ask_user"
)

// controlTool records one phase transition. Validation failures come back as
// error results rather than Go errors, so the model corrects itself inside the
// same turn; only a recorded transition ends the turn.
type controlTool struct {
	c      *Controller
	name   string
	label  string
	desc   string
	from   Phase // the only phase this tool may be called in
	params json.RawMessage
	record func(*Controller, json.RawMessage) (string, error)
}

var _ agent.Tool = (*controlTool)(nil)

func (t *controlTool) Name() string                { return t.name }
func (t *controlTool) Label(agent.ToolCall) string { return t.label }
func (t *controlTool) Description() string         { return t.desc }
func (t *controlTool) Schema() llm.ToolSchema      { return llm.ToolSchema{Parameters: t.params} }
func (t *controlTool) Mode() agent.ExecutionMode   { return agent.ModeSerial }

// Execute records the transition and hands the turn over, or explains why it
// could not.
func (t *controlTool) Execute(_ context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()

	switch {
	case !t.c.phase.active():
		return controlErr(t.name + " is only valid inside a running plan workflow"), nil
	case t.c.phase != t.from:
		return controlErr(t.name + " is only valid in the " + t.from.String() +
			" phase; the workflow is " + t.c.phase.String()), nil
	case t.c.pending != nil:
		return controlErr("a phase transition is already pending this turn; " +
			"call only one control tool per turn"), nil
	}

	msg, err := t.record(t.c, call.Input)
	if err != nil {
		return controlErr(err.Error()), nil
	}
	return agent.ToolResult{
		Content: llm.BlockList{llm.TextBlock{Text: msg}},
		Display: msg,
		EndTurn: true, // the next phase owns the conversation from here
	}, nil
}

// controlErr frames a rejected control call so the model can retry in-turn.
func controlErr(msg string) agent.ToolResult {
	return agent.ToolResult{
		Content: llm.BlockList{llm.TextBlock{Text: msg}},
		Display: msg,
		IsError: true,
	}
}

type implementParams struct {
	Plan string `json:"plan"`
}

type reviewParams struct {
	Summary string `json:"summary"`
}

type reviseParams struct {
	Instructions string `json:"instructions"`
}

// Parameter schemas are written out rather than derived: four fixed shapes are
// clearer here than a reflection helper this package would otherwise not need.
var (
	implementSchema = json.RawMessage(`{"type":"object","properties":{"plan":` +
		`{"type":"string","description":"the complete, self-contained implementation plan"}},` +
		`"required":["plan"],"additionalProperties":false}`)
	reviewSchema = json.RawMessage(`{"type":"object","properties":{"summary":` +
		`{"type":"string","description":"what you changed and anything the reviewer ` +
		`must know; the reviewer sees none of this conversation"}},` +
		`"required":["summary"],"additionalProperties":false}`)
	reviseSchema = json.RawMessage(`{"type":"object","properties":{"instructions":` +
		`{"type":"string","description":"specific, self-contained revision instructions"}},` +
		`"required":["instructions"],"additionalProperties":false}`)
	completeSchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
)

// controlTools builds the dev_* set bound to c, in registration order.
func controlTools(c *Controller) []agent.Tool {
	return []agent.Tool{
		&controlTool{
			c: c, name: DevImplementTool, label: "plan: hand off to implementation",
			from: PhasePlanning, params: implementSchema,
			desc: "Finalise the plan and hand it to the user for approval. Call this only when " +
				"the plan is complete and every clarifying question is resolved. The plan is " +
				"shown to the user to edit, then given to a separate implementation model with " +
				"no prior context, so it must carry every file path, interface and constraint. " +
				"Your turn ends here.",
			record: recordPlan,
		},
		&controlTool{
			c: c, name: DevReviewTool, label: "implement: request review",
			from: PhaseImplementing, params: reviewSchema,
			desc: "Signal that the implementation is complete and hand off to review. The " +
				"summary is how the reviewer learns what you did — it sees none of this " +
				"conversation, only the plan and what you report here, so cover what you " +
				"changed, anything you could not do, and anything it must check. Review " +
				"begins automatically if you stop without calling this, so call it only " +
				"when the work is done.",
			record: recordSummary,
		},
		&controlTool{
			c: c, name: DevReviseTool, label: "review: request revisions",
			from: PhaseReviewing, params: reviseSchema,
			desc: "Send the implementation back with specific, actionable instructions. They go " +
				"to a model with no prior context and no background, so each instruction must be " +
				"self-contained and name the files it concerns.",
			record: recordRevision,
		},
		&controlTool{
			c: c, name: DevCompleteTool, label: "review: accepted and done",
			from: PhaseReviewing, params: completeSchema,
			desc: "Accept the implementation as fulfilling the plan and end the workflow, " +
				"restoring the model and tool set the session started with.",
			record: recordComplete,
		},
	}
}

// recordPlan captures the drafted plan and parks the workflow for user approval.
func recordPlan(c *Controller, raw json.RawMessage) (string, error) {
	var p implementParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errBadArgs(err)
	}
	if strings.TrimSpace(p.Plan) == "" {
		return "", errEmpty("plan")
	}
	c.pending = &transition{to: PhaseAwaitingPlan, payload: p.Plan}
	return "Plan recorded. It goes to the user to review and edit before implementation starts.", nil
}

// recordSummary captures the implementor's self-summary and hands off to review.
func recordSummary(c *Controller, raw json.RawMessage) (string, error) {
	var p reviewParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errBadArgs(err)
	}
	if strings.TrimSpace(p.Summary) == "" {
		return "", errEmpty("summary")
	}
	c.pending = &transition{to: PhaseReviewing, payload: p.Summary}
	return "Implementation marked complete. Review starts next.", nil
}

// recordRevision captures review instructions for another implementation round.
func recordRevision(c *Controller, raw json.RawMessage) (string, error) {
	var p reviseParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", errBadArgs(err)
	}
	if strings.TrimSpace(p.Instructions) == "" {
		return "", errEmpty("instructions")
	}
	c.pending = &transition{to: PhaseImplementing, payload: p.Instructions}
	return "Revision instructions recorded. A fresh implementation round starts next.", nil
}

// recordComplete accepts the implementation and ends the workflow.
func recordComplete(c *Controller, _ json.RawMessage) (string, error) {
	c.pending = &transition{to: PhaseDone}
	return "Workflow complete. Restoring the original model and tool set.", nil
}

// errBadArgs reports unparseable tool arguments.
func errBadArgs(err error) error { return errors.New("bad args: " + err.Error()) }

// errEmpty reports a required field the model left blank.
func errEmpty(field string) error { return errors.New(field + " must not be empty") }
