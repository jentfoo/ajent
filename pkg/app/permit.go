package app

import (
	"context"
	"errors"
	"os"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

type promptAdapter struct {
	ui   *tui.UI
	hold func(context.Context) // typing wait behind Hold
}

// Hold defers dialogs while the user is typing a message.
func (a promptAdapter) Hold(ctx context.Context) {
	a.hold(ctx)
}

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
		// an explicit Esc is a real denial, not the headless no-UI path
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

// barrierOptions carries what one front end supplies to the shared barrier
// construction: the model classifier inputs, config lists and the notify sink.
// Mode, grants, prompter, noter and preview live in permit.Options, which this
// embeds and completes.
type barrierOptions struct {
	reg         *tools.Registry
	providerFor func(llm.Model) (llm.Provider, error)
	model       func() llm.Model
	session     string
	notify      func(msg string, level agent.Level)
	safe        []string
	denied      []string
}

// options returns the decision inputs both front ends share: classifier,
// workspace write roots, config safe/deny lists, dry-run and notices. o's
// caller-set fields (mode, grants, prompter, noter, preview) pass through.
func (d barrierOptions) options(o permit.Options) permit.Options {
	o.Classifier = permit.NewCachedClassifier(classifierAdapter{
		providerFor: d.providerFor,
		model:       d.model,
		schema:      toolSchema(d.reg),
		cwd:         config.Cwd(),
		tmp:         os.TempDir(),
		session:     d.session,
	}.Classify)
	o.WriteRoots = []string{config.Cwd(), os.TempDir()}
	o.SafeCommands = d.safe
	o.DeniedCommands = d.denied
	o.DryRun = d.reg.DryRun
	o.Notice = func(msg string) { d.notify(msg, agent.LevelInfo) }
	return o
}

// install gates reg behind the barrier's guard and asker.
func (d barrierOptions) install(b *permit.Barrier) {
	d.reg.AddGuard(b.Guard())
	d.reg.SetAsker(b.Asker())
}

// reserve is the small slice of the sub-agent manager the batch hook needs.
type reserve interface {
	Reserve(calls []agent.ToolCall)
}

// wireBatchHooks sets opts.OnToolBatch to reserve sub-agent job numbers and
// prefetch classifications for one step's calls, both in message order.
func wireBatchHooks(opts *agent.Options, sag reserve, b *permit.Barrier) {
	opts.OnToolBatch = func(ctx context.Context, calls []agent.ToolCall) {
		if sag != nil {
			sag.Reserve(calls)
		}
		b.Prefetch(ctx, calls)
	}
}

type classifierAdapter struct {
	providerFor func(llm.Model) (llm.Provider, error)
	model       func() llm.Model
	schema      func(name string) (llm.ToolSchema, bool) // MCP tool metadata lookup
	cwd         string
	tmp         string
	session     string
}

func (a classifierAdapter) Classify(ctx context.Context, s permit.Subject) permit.Class {
	m := a.model()
	if m.ID == "" { // no model configured, nothing to classify with
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
		// constant prompt re-sent per gated call, needs SessionID on openai
		Cache:     llm.CachePolicy{Enabled: true},
		SessionID: a.session,
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
		ui.SetStatusSegment(segment(segPermissions, m.String(), m.Short()))
		return
	}
	ui.SetStatusSegment(tui.Segment{Key: segPermissions}) // empty Text removes it
}
