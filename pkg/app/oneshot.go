package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/mcp"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/refs"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
)

// RunHeadless drives one turn with no terminal and returns the process exit
// code. Out, Errw and Provider default to stdout, stderr and the registry.
func RunHeadless(o HeadlessOptions) int {
	started := time.Now()
	out, errw := o.Out, o.Errw
	if out == nil {
		out = os.Stdout
	}
	if errw == nil {
		errw = os.Stderr
	}
	for _, w := range o.Warnings {
		_, _ = fmt.Fprintln(errw, w)
	}
	if o.Active.ID == "" {
		_, _ = fmt.Fprintln(errw, "no model configured; set one with -m or in config.json")
		return ExitUsage
	}

	var drain headSink
	if o.Output == OutputJSON {
		drain = newJSONSink(out)
	} else {
		drain = newTextSink(out, errw)
	}
	notify := func(msg string, level agent.Level) {
		_, _ = fmt.Fprintf(errw, "%s: %s\n", levelName(level), msg)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	providerFor := o.Provider
	if providerFor == nil {
		providerFor = llm.NewProviders(o.Reg).ProviderFor
	}
	st := &agent.State{
		Model:     o.Active,
		Reasoning: llm.ReasoningFrom(o.Set.Settings().Reasoning, o.Active),
		Tokens:    tokens.New(o.Active),
	}

	// recording keeps -p composable: a follow-up --continue rejoins this transcript
	rec, serr := newSession(nil, o.SessMode, o.SessTarget, o.Active.Key())
	if serr != nil && o.SessMode != ResumeNewSession {
		// an explicit resume target that cannot open must not silently start fresh
		_, _ = fmt.Fprintf(errw, "session: %v\n", serr)
		return ExitUsage
	}
	if serr != nil {
		// a fresh start that cannot record still runs: a one-shot is unattended,
		// and persistence failures degrade to "not recorded", never fail the run
		notify("session: "+serr.Error(), agent.LevelWarn)
	}
	if rec == nil {
		notify("session recording disabled", agent.LevelWarn)
	}

	// ask_user has nobody to ask, so it is left without an Ask func and excluded from every scope below
	toolsReg, terr := builtinTools(o.Set, o.Reg, nil, func(msg string) {
		_, _ = fmt.Fprintln(errw, msg)
	})
	if terr != nil {
		_, _ = fmt.Fprintln(errw, terr)
		return ExitUsage
	}

	env := agent.DetectEnvironment()
	globalDir, _ := config.Home()
	proj, perr := agent.LoadProjectInstructions(globalDir, env.Cwd)
	if perr != nil {
		notify("could not read AGENTS.md: "+perr.Error(), agent.LevelWarn)
	}

	var stats *statsSink
	if o.Stats {
		stats = newStatsSink()
	}

	var comp *compactor
	opts := agent.Options{
		Sinks:               []agent.Sink{drain},
		Env:                 env,
		ProjectInstructions: proj,
		SystemPrompt:        o.Set.Settings().Agent.SystemPrompt, // config or --system, replaces default guidance
		Tools:               toolsReg,
		Provider:            providerFor,
		Compact: func(ctx context.Context, reason agent.CompactReason) (bool, error) {
			if comp == nil {
				return false, nil // recording is off, nothing to compact
			}
			return comp.run(ctx, reason, "")
		},
		TurnBoundary: func() {
			if comp != nil {
				comp.endTurn()
			}
		},
		MaxSteps:    o.Set.Settings().Agent.MaxSteps,
		TurnRetries: o.Set.Settings().Agent.TurnRetries,
		SessionID:   sessionHint(rec),
	}
	if rec != nil {
		opts.Sinks = []agent.Sink{rec.rec.Sink(drain)}
		opts.OnMessage = []func(agent.MessageInfo){rec.rec.Message}
		_, _, warns := rec.restoreState(o.Set, o.Reg, st, toolsReg)
		for _, w := range warns {
			notify("resume: "+w, agent.LevelWarn)
		}
		if st.Model.ID != "" {
			o.Reg.SetActive(st.Model) // the branch may name a model resolveActiveModel never saw
		}
	}
	if stats != nil {
		// a resumed ledger already carries prior spend, so fix the baseline before
		// the first turn and the summary reports only this invocation's work
		stats.baseline(st.Tokens)
	}

	var ag *agent.Agent
	sag := subagent.New(subagent.Options{
		Provider:            providerFor,
		Model:               func() llm.Model { return resolveSubAgentModel(o.Set, o.Reg, st) },
		Reasoning:           func() llm.ReasoningConfig { return st.Reasoning },
		Parent:              func() *tokens.Accounting { return st.Tokens },
		Tools:               toolsReg,
		Env:                 env,
		ProjectInstructions: proj,
		Root:                ctx, // job contexts die with the run's signal-notified root
		Notice:              func(msg string) { notify(msg, agent.LevelInfo) },
		Deliver: func(in agent.Input) bool {
			if ag == nil || !ag.Running() {
				return false
			}
			return ag.Steer(in)
		},
		MaxConcurrent: o.Set.Settings().Subagent.MaxConcurrent,
		PollTimeout: subagentPollWait(o.Set, func(msg string) {
			notify(msg, agent.LevelWarn)
		}),
		MaxSteps: o.Set.Settings().Subagent.MaxSteps,
	})
	defer sag.Close()
	for _, t := range sag.Tools() {
		toolsReg.RegisterFrom(tools.SourceBuiltin, t, true)
	}
	toolsReg.MarkReadOnly([]string{subAgentToolStart, subAgentToolPoll, subAgentToolList})
	if stats != nil { // appended after the recorder settles the drain list
		opts.Sinks = append(opts.Sinks, stats)
	}
	opts.Sinks = append(opts.Sinks, subagentSink{mgr: sag})
	// headless runs have no queued prompts, so the boundary hook serves completions
	// alone, deciding membership when the message lands so polls are never duplicated
	opts.OnBoundary = sag.Boundary

	servers, mwarns, merr := mcp.LoadConfig(config.Cwd())
	if merr != nil {
		notify("mcp: "+merr.Error(), agent.LevelWarn)
	}
	for _, w := range mwarns {
		notify("mcp: "+w, agent.LevelWarn)
	}
	if merr == nil {
		mgr := mcp.New(servers, mcp.Options{
			Registrar: registryAdapter{toolsReg},
			Workspace: config.Cwd(),
			Restore:   st.Tools,
			Notice:    func(msg string, warn bool) { notify("mcp: "+msg, agentLevelOf(warn)) },
		})
		defer mgr.Close()
		mgr.LoadOnFirstMessage(ctx) // every remote tool must exist before the scope is applied
	}

	// the scope is applied last, once every tool the run could offer is registered
	toolsReg.SetEnabled(headlessTools(toolsReg, o.Scope, o.AllowTools, o.DenyTools))

	// a one-shot settles every ask without a dialog: the auto modes take the model
	// verdict as final, so the default gates writes rather than running allow-all.
	// Precedence: permission flag > permissions.headlessMode config > auto+write.
	m, ok := o.Scope.barrierMode()
	if !ok {
		m = permit.ModeAutoWrite
		if hm := o.Set.Settings().Permissions.HeadlessMode; hm != "" {
			if parsed, pok := permit.ParseMode(hm); pok {
				m = parsed
			} else {
				notify("unknown permissions.headlessMode "+hm+", using auto+write", agent.LevelWarn)
			}
		}
	}
	// --allow-tools names pre-granted session allows: a tool name runs that tool
	// (writers included), `bash` every nameable shell call, any other word a bash
	// command head. They hold across the mode changes that clear earned grants.
	for _, n := range o.AllowTools {
		// a grant keys a tool name or one head word, so a multi-word entry can only
		// be a mistake; say so rather than pass silently
		if len(strings.Fields(n)) > 1 {
			notify("--allow-tools: "+n+" names no tool; use a tool name or one bash command head",
				agent.LevelWarn)
		}
	}
	bd := barrierOptions{
		reg:         toolsReg,
		providerFor: providerFor,
		model:       func() llm.Model { return st.Model },
		session:     sessionHint(rec),
		notify:      notify,
		safe:        o.Set.Settings().Permissions.SafeCommands,
		denied:      o.Set.Settings().Permissions.DeniedCommands,
	}
	barrier := permit.NewBarrier(toolsReg.ReadOnly, bd.options(permit.Options{
		Mode: m, ModeSet: true, Grants: o.AllowTools,
	}))
	bd.install(barrier)
	// a batch's classified calls launch concurrently and agent_start calls reserve
	// message-order job numbers, the same hooks the interactive driver installs
	wireBatchHooks(&opts, sag, barrier)

	// steered inputs (sub-agent completions) expand through the same @ pipeline
	// the initial prompt gets, via the agent's append-point seam. Vision reads
	// the registry's active model, the read tool's own gate source.
	expander := refs.NewExpander(toolsReg, opts.Sinks[0], tools.PathPolicy{Cwd: config.Cwd()},
		func() bool { return o.Reg.Active().Caps.Images })
	opts.NormalizeInput = func(in agent.Input) agent.Input {
		return refs.Normalize(expander, in, func(n string) { notify(n, agent.LevelWarn) })
	}

	ag = agent.New(st, opts)
	// a resumed ledger carries no base of its own, and buildRequest reads Used for
	// MaxOutputFor before stream() seeds one. A one-shot's tool set is fixed by its
	// flags, so the block is committed from the start.
	st.Tokens.SetBase(ag.BaseEstimate(true))

	if rec != nil {
		comp = &compactor{
			rec: rec, st: st, ag: ag, reg: o.Reg,
			sink:        opts.Sinks[0],
			notify:      notify,
			providerFor: providerFor,
			cfg:         func() config.Compaction { return o.Set.Settings().Compaction },
		}
	}

	// expand @ references like the pump does, once the scope has settled
	expander.Seed(st.Messages) // --continue reopens a transcript that holds ref ids
	expanded := expander.Expand(o.Prompt)
	for _, n := range expanded.Notices {
		notify(n, agent.LevelWarn)
	}
	err := ag.Prompt(ctx, agent.Input{
		Text: expanded.Text, After: expanded.Run, Prepared: true,
	})
	toolsReg.Close() // kill background commands so none outlives the run
	answer := llm.FinalAnswer(st.Messages)
	res := drain.result()
	status, code := headlessOutcome(err, res, answer)
	if status != statusOK {
		// text output prints nothing without an answer, so say why on stderr
		_, _ = fmt.Fprintln(errw, outcomeReason(err, res))
	}
	if stats != nil {
		drain.summary(stats.collect(st.Tokens, time.Since(started)))
	}
	drain.finish(status, code, answer)
	return code
}

// outcomeReason explains a non-zero exit in one line.
func outcomeReason(err error, res agent.TurnResult) string {
	switch {
	case err != nil:
		return err.Error()
	case res.Err != nil:
		return res.Err.Error()
	case res.Stop == llm.StopAborted:
		return "interrupted before an answer"
	default:
		return "the model produced no final answer (stop: " + res.Stop.String() + ")"
	}
}

// headlessOutcome maps how a turn ended onto its json status and exit code.
func headlessOutcome(err error, res agent.TurnResult, answer string) (string, int) {
	switch {
	case err != nil || res.Err != nil || res.Stop == llm.StopError:
		return statusError, ExitTurn
	case res.Stop == llm.StopAborted:
		return statusEmpty, ExitTurn
	case answer == "":
		return statusEmpty, ExitTurn
	default:
		return statusOK, ExitOK
	}
}

// headlessTools returns the tool names to enable for scope, then applies the allow and deny
// adjustments. Built-in names follow the scope regardless of tools.enabled, and every other
// source keeps its registered state unless --allow-tools names it, so an unlisted server
// disabled in mcp.json stays off.
func headlessTools(reg *tools.Registry, scope ToolScope, allow, deny []string) []string {
	inScope := func(name string) bool {
		if name == tools.ToolAskUser { // no human to answer a question headless
			return false
		}
		switch scope {
		case ToolScopeReadOnly:
			return slices.Contains(tools.ReadOnlyBuiltins, name) || reg.ReadOnly(name)
		case ToolScopeAuto:
			// core writers have no unattended path: never offered rather than refused
			return !permit.IsCoreWriter(name)
		default:
			// the default scope is auto+write, whose gate covers bash and the writers
			return true
		}
	}

	builtins := reg.AllNames(tools.SourceBuiltin)
	builtinSet := bulk.SliceToSet(builtins)
	names := bulk.SliceFilterInPlace(inScope, builtins)

	// enabled non-builtins keep their state, narrowed to read-only under that scope
	names = append(names, bulk.SliceFilterInPlace(func(name string) bool {
		if _, ok := builtinSet[name]; ok {
			return false
		}
		return scope != ToolScopeReadOnly || reg.ReadOnly(name)
	}, reg.Names())...)

	names = append(names, bulk.SliceFilter(func(n string) bool {
		// only a registered tool can be enabled; other entries stay bash prefixes
		_, ok := reg.Lookup(n)
		return ok
	}, allow)...)
	denied := bulk.SliceToSet(deny)
	names = bulk.SliceFilterInPlace(func(n string) bool {
		_, ok := denied[n]
		return !ok
	}, names)
	return slices.Compact(slices.Sorted(slices.Values(names)))
}
