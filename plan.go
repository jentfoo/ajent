package main

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/plan"
	"github.com/jentfoo/ajent/pkg/session"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// gitTimeout bounds the working-tree capture handed to a review round. The two
// commands run concurrently, so this is the whole wait the drain goroutine takes
// between the implementation turn and the review turn.
const gitTimeout = 5 * time.Second

// planDeps is what the plan Host is assembled from, gathered so driver's wiring
// block stays one call.
type planDeps struct {
	rec      *sessRec
	ag       *agent.Agent
	reg      *llm.Registry
	st       *agent.State
	ui       *tui.UI
	console  *uiConsole
	toolsReg *tools.Registry
	q        *steerQueue
}

// newPlanController builds the workflow controller over the live driver. It
// returns nil when the session or tool registry is missing, since a workflow
// needs a transcript to branch and a registry to scope.
func newPlanController(d planDeps) *plan.Controller {
	if d.rec == nil || d.toolsReg == nil {
		return nil
	}
	return plan.New(plan.Host{
		PickModel: func(ctx context.Context, title string) (llm.Model, bool) {
			if d.ui.Mode() == tui.ModePlain {
				return llm.Model{}, false
			}
			m, err := command.PickModel(ctx, d.console, title, d.st.Model.Key(),
				tui.PickOptions{Silent: true})
			return m, err == nil
		},
		ActiveModel: func() llm.Model { return d.st.Model },
		Running:     d.ag.Running,
		Abort: func() {
			d.q.abort() // queued prompts return to the editor rather than the next phase
			d.ag.Interrupt()
		},

		ToolNames:    d.toolsReg.Names,
		PlannerTools: func() []string { return plannerExtras(d.toolsReg) },
		SetTools: func(names []string) {
			// a phase scope narrows deliberately, which /tools refuses to do after the
			// first prompt; it is user-initiated and restored on every exit path.
			d.toolsReg.SetEnabled(names)
			d.console.ToolsChanged()
		},
		AddTools: func(ts []agent.Tool) {
			for _, t := range ts {
				d.toolsReg.RegisterFrom(plan.Source, t, true)
			}
			d.toolsReg.RegisterGroup(tools.ToolGroup{
				Name:   "plan",
				Source: plan.Source,
				Tools: []string{plan.DevImplementTool, plan.DevReviewTool,
					plan.DevReviseTool, plan.DevCompleteTool},
			})
			// control calls change nothing on disk, so they never need approval
			d.toolsReg.MarkReadOnly([]string{plan.DevImplementTool, plan.DevReviewTool,
				plan.DevReviseTool, plan.DevCompleteTool})
		},
		DropTools: func() { d.toolsReg.Unregister(plan.Source) },

		Fork: func(head string, m llm.Model) error { return d.rec.forkTo(d.ui, d.ag, d.reg, head, m) },
		Head: d.rec.w.Head,

		Persist: func(v any) error { return d.rec.rec.Custom(plan.CustomType, v) },
		Restore: func(v any) bool {
			entries, _, err := session.Read(d.rec.w.Path())
			if err != nil || len(entries) == 0 {
				return false
			}
			branch := session.Branch(entries, resumeHead(d.rec.w.Head(), entries))
			return session.LatestCustom(branch, plan.CustomType, v)
		},
		ResolveModel: func(key string) (llm.Model, bool) {
			m, err := d.reg.Resolve(key)
			return m, err == nil
		},

		LastText: func() string { return lastAssistantText(d.st.Messages) },

		SetInput: d.ui.SetInput,
		Ask: func(ctx context.Context, q string, opts []string) (int, error) {
			options := make([]tui.Option, len(opts))
			for i, o := range opts {
				options[i] = tui.Option{Label: o}
			}
			return d.ui.SelectContext(ctx, q, options)
		},
		Notify: func(msg string, level agent.Level) { d.ui.Notify("plan: "+msg, tuiLevel(level)) },
		Status: func(text, short string) {
			d.ui.SetStatusSegment(tui.Segment{Key: "plan", Text: text, Short: short})
		},
		Git: gitState,
	})
}

// planHooksFor adapts the controller onto the pump and drain seams, reading the
// last turn's outcome from turnRec at the boundary.
func planHooksFor(ctl *plan.Controller, turnRec *turnRecorder) planHooks {
	if ctl == nil {
		return planHooks{}
	}
	return planHooks{
		beforePrompt: ctl.BeforePrompt,
		advance: func(ctx context.Context) (agent.Input, bool) {
			return ctl.Advance(ctx, turnRec.last())
		},
	}
}

// planCommands are the workflow's slash commands, registered alongside the
// built-ins rather than in place of them.
func planCommands(ctl *plan.Controller) []command.Command {
	if ctl == nil {
		return nil
	}
	return []command.Command{
		{
			Name:        "plan",
			Description: "start a two-model plan, implement and review workflow",
			Args:        "[goal]",
			Handler: func(ctx context.Context, args string, c command.Console) error {
				if msg := ctl.Start(ctx, strings.TrimSpace(args)); msg != "" {
					c.Notify("plan: "+msg, tui.LevelWarn)
				}
				return nil
			},
		},
		{
			Name:        "plan-stop",
			Description: "stop the plan workflow and restore the original model and tools",
			Handler: func(context.Context, string, command.Console) error {
				ctl.Stop()
				return nil
			},
		},
		{
			Name:        "plan-status",
			Description: "show the plan workflow's phase, round and models",
			Handler: func(_ context.Context, _ string, c command.Console) error {
				status := ctl.Status()
				if status == "" {
					status = "no plan workflow is running"
				}
				c.Notify("plan: "+status, tui.LevelInfo)
				return nil
			},
		},
	}
}

// forkTo points the session and agent state at head's branch and applies m as
// its model. An empty head starts a new root, whose first entry is the model
// change so a resumed branch can resolve it.
func (r *sessRec) forkTo(ui *tui.UI, ag *agent.Agent, reg *llm.Registry, head string, m llm.Model) error {
	moved := head != r.w.Head() // a fork in place only records the model, nothing to divide
	if err := r.switchState(ui, ag, reg, head, "plan: "); err != nil {
		return err
	}
	r.rec.ModelChange(m, "plan")
	reg.SetActive(m)
	ag.WithState(func(st *agent.State) {
		st.Model = m
		if st.Tokens != nil {
			st.Tokens.SetModel(m)
			st.Tokens.Reseed(tokens.EstimateMessages(st.Messages))
		}
		pushSwitchedContext(ui, st)
	})
	ui.SetModel(m.Key(), m.ContextWindow)
	if moved {
		ui.Divider() // phase boundaries read as breaks in scrollback
	}
	return nil
}

// plannerExtras returns the enabled sub-agent tools a planner may delegate
// read-only research to, empty when none are registered.
func plannerExtras(reg *tools.Registry) []string {
	all := reg.AllNames(tools.SourceBuiltin)
	var out []string
	for _, n := range []string{"agent_start", "agent_poll", "agent_list"} {
		if slices.Contains(all, n) {
			out = append(out, n)
		}
	}
	return out
}

// gitState captures the working tree for a review round. Failures are reported
// in place rather than blocking the review.
func gitState(ctx context.Context) (status, diffStat string) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); status = runGit(ctx, "status", "--porcelain") }()
	go func() { defer wg.Done(); diffStat = runGit(ctx, "diff", "--stat") }()
	wg.Wait()
	return status, diffStat
}

// runGit returns the trimmed stdout of one git invocation, or a parenthesised
// note when it could not run.
func runGit(ctx context.Context, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return "(git " + args[0] + " unavailable: " + err.Error() + ")"
	}
	return strings.TrimRight(string(out), "\n")
}

// tuiLevel maps an agent notice level onto the UI's.
func tuiLevel(l agent.Level) tui.Level {
	switch l {
	case agent.LevelWarn:
		return tui.LevelWarn
	case agent.LevelError:
		return tui.LevelError
	default:
		return tui.LevelInfo
	}
}

// askUser adapts the TUI onto the ask_user tool's asker. It reports declined on
// Esc and an error when no terminal can reach the user.
func askUser(ui *tui.UI) tools.AskFunc {
	return func(ctx context.Context, question string, opts []string) (int, string, bool, error) {
		if ui == nil || ui.Mode() == tui.ModePlain {
			return 0, "", false, tui.ErrNoUI
		}
		options := make([]tui.Option, len(opts))
		for i, o := range opts {
			options[i] = tui.Option{Label: o}
		}
		ans, err := ui.Ask(ctx, tui.Question{Text: question, Options: options})
		if err != nil {
			return 0, "", false, err
		} else if ans.Chat { // replied in their own words rather than choosing
			return -1, ans.Text, false, nil
		}
		return ans.Index, ans.Text, ans.Declined, nil
	}
}

// lastAssistantText returns the text of the newest assistant message, empty when
// the context holds none.
func lastAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleAssistant {
			continue
		}
		var b strings.Builder
		for _, blk := range msgs[i].Content {
			if tb, ok := blk.(llm.TextBlock); ok {
				b.WriteString(tb.Text)
			}
		}
		if text := strings.TrimSpace(b.String()); text != "" {
			return text
		}
	}
	return ""
}
