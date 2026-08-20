package tools

import (
	"context"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// AskFunc puts question to the user and returns the chosen option index, or
// declined when they dismissed it. An empty options slice asks for free text,
// returned in place of an index.
type AskFunc func(ctx context.Context, question string, options []string) (index int, text string, declined bool, err error)

// askParams is the model-facing parameter block for ask_user.
type askParams struct {
	Question string   `json:"question" desc:"the question to put to the user"`
	Options  []string `json:"options,omitempty" desc:"choices to offer; omit for a free-text answer"`
}

// askUserTool puts a decision that is genuinely the user's back to them.
// Registered off by default; a workflow that wants it enables it.
type askUserTool struct {
	ask AskFunc
}

var _ agent.Tool = (*askUserTool)(nil)

func (t *askUserTool) Name() string { return "ask_user" }

func (t *askUserTool) Label(call agent.ToolCall) string {
	var p askParams
	if err := decode(call.Input, &p); err != nil || strings.TrimSpace(p.Question) == "" {
		return "ask_user"
	}
	return "ask: " + strings.TrimSpace(strutil.FirstLine(p.Question))
}

func (t *askUserTool) Description() string {
	return "Ask the user a question and wait for their answer. Use it only when a decision is " +
		"genuinely theirs — a design fork with real trade-offs, or a fact you cannot discover. " +
		"Never ask for permission to act, and never ask what you could determine by reading the " +
		"project. Offer options when the choice is closed, omit them for free text."
}

func (t *askUserTool) Schema() llm.ToolSchema {
	return llm.ToolSchema{Parameters: SchemaOf[askParams]()}
}

// Mode is serial: a question owns the terminal until it is answered.
func (t *askUserTool) Mode() agent.ExecutionMode { return agent.ModeSerial }

// Execute returns the answer, or a normal result explaining that no answer came.
// A declined or unavailable question is never an error, so a headless run keeps
// going instead of failing the turn.
func (t *askUserTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p askParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	question := strings.TrimSpace(p.Question)
	if question == "" {
		return resultErr("question must not be empty"), nil
	}
	if t.ask == nil {
		return askAnswer("ask_user is unavailable: there is no interactive terminal. " +
			"Decide for yourself and state the assumption you made."), nil
	}

	index, text, declined, err := t.ask(ctx, question, p.Options)
	if err != nil {
		return askAnswer("ask_user could not reach the user (" + err.Error() +
			"). Decide for yourself and state the assumption you made."), nil
	}
	if declined {
		return askAnswer("The user declined to answer. Decide for yourself and state the " +
			"assumption you made."), nil
	}
	if len(p.Options) > 0 {
		if index < 0 || index >= len(p.Options) {
			return askAnswer("The user's choice could not be read; treat the question as unanswered."), nil
		}
		return askAnswer("The user chose: " + p.Options[index]), nil
	}
	if strings.TrimSpace(text) == "" {
		return askAnswer("The user answered with nothing. Decide for yourself and state the " +
			"assumption you made."), nil
	}
	return askAnswer("The user answered: " + text), nil
}

// askAnswer frames an answer as both model content and history display.
func askAnswer(text string) agent.ToolResult {
	return agent.ToolResult{Content: llmBlock(text), Display: text}
}
