package app

import (
	"context"
	"errors"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

type promptAdapter struct{ ui *tui.UI }

func (a promptAdapter) Open(prompt, subject string, options []string) (permit.Dialog, error) {
	if a.ui.Mode() == tui.ModePlain { // nobody to ask in headless/piped mode
		return nil, tui.ErrNoUI
	}
	opts := make([]tui.Option, len(options))
	for i, o := range options {
		opts[i] = tui.Option{Label: o}
	}
	d := a.ui.OpenDecision(tui.DecisionRequest{Prompt: prompt, Context: subject, Options: opts})
	return decisionAdapter{d}, nil
}

func (a promptAdapter) Reason(ctx context.Context, label string) (string, bool) {
	ans, err := a.ui.Ask(ctx, tui.Question{Text: label})
	if err != nil || ans.Declined {
		return "", false
	}
	return ans.Text, true
}

type decisionAdapter struct{ d *tui.Decision }

func (a decisionAdapter) Wait(ctx context.Context) (int, error) {
	r, err := a.d.Wait(ctx)
	if err != nil {
		// an explicit Esc is a real denial, not the headless no-UI path.
		if errors.Is(err, tui.ErrCancelled) {
			return 0, permit.ErrDenied
		}
		return 0, err
	}
	return r.Index, nil
}

func (a decisionAdapter) Resolve(index int) { a.d.Resolve(index) }

func (a decisionAdapter) Close() { a.d.Close() }

func toolSchema(reg *tools.Registry) func(name string) (llm.ToolSchema, bool) {
	return func(name string) (llm.ToolSchema, bool) {
		t, ok := reg.Get(name)
		if !ok {
			return llm.ToolSchema{}, false
		}
		sch := t.Schema()
		sch.Name = name
		sch.Description = t.Description()
		return sch, true
	}
}

type classifierAdapter struct {
	providerFor func(llm.Model) (llm.Provider, error)
	model       func() llm.Model
	schema      func(name string) (llm.ToolSchema, bool) // nil: no MCP metadata available
	cwd         string
	tmp         string
}

func (a classifierAdapter) Classify(ctx context.Context, s permit.Subject) permit.Class {
	if a.schema == nil && !s.IsShell() {
		return permit.ClassUnsure // an MCP call needs its tool metadata to be judged
	}
	m := a.model()
	if m.ID == "" { // no model configured; nothing to classify with
		return permit.ClassUnsure
	}
	p, err := a.providerFor(m)
	if err != nil {
		return permit.ClassUnsure
	}
	sys := permit.ClassifierSystem
	switch {
	case !s.IsShell(): // an MCP/extension tool call is judged with its own framing and metadata
		sch, ok := a.schema(s.Name)
		if !ok {
			return permit.ClassUnsure // unknown tool cannot be evaluated safely
		}
		sys = permit.MCPClassifierSystem(sch.Name, sch.Description, string(sch.Parameters))
	case s.AllowWrite: // auto+write permits writes confined to the workspace
		sys = permit.WorkspaceClassifierSystem(a.cwd, a.tmp)
	}
	userMsg := s.Args
	if s.Cwd != "" { // a shell cwd rebases every relative path in the command
		userMsg = "working directory: " + s.Cwd + "\n" + userMsg
	}
	req := llm.Request{
		Model:     m,
		System:    llm.BlockList{llm.TextBlock{Text: sys}},
		Messages:  []llm.Message{{Role: llm.RoleUser, Content: llm.BlockList{llm.TextBlock{Text: userMsg}}}},
		MaxTokens: classifyBudget(m),
	}
	req.Reasoning = llm.ReasoningConfig{Level: llm.ClampLevel(m, llm.LevelOff)}
	out, _, serr := llm.RunSummary(ctx, p, req)
	if serr != nil {
		return permit.ClassUnsure
	}
	return permit.NormalizeClass(out)
}

func classifyBudget(m llm.Model) int {
	budget := m.MaxOutput
	if budget <= 0 {
		return 4096 // unknown window: modest fallback, enough for one word plus thought
	}
	return min(budget, 20480)
}

func showPermissionIndicator(ui *tui.UI, b *permit.Barrier) {
	if ui == nil || b == nil {
		return
	}
	m := b.Mode()
	if m != permit.ModeAllowRead { // default: hidden until the user changes it
		ui.SetStatusSegment(tui.Segment{Key: "permissions", Text: m.String(), Short: m.Short()})
		return
	}
	ui.SetStatusSegment(tui.Segment{Key: "permissions"})
}
