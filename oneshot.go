package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
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

// toolScope is which tools a headless run offers the model. The scope is the
// gate: the barrier runs at allow-all, so nothing the model can see is refused.
type toolScope uint8

const (
	scopeDefault  toolScope = iota // every built-in but bash
	scopeAllowAll                  // every built-in, bash included
	scopeReadOnly                  // verifiably read-only tools only
)

// headlessOptions is everything main resolved before deciding not to open a UI.
// out, errw and provider default to stdout, stderr and the registry.
type headlessOptions struct {
	flags      cliFlags
	set        *config.Set
	reg        *llm.Registry
	active     llm.Model
	sessMode   resumeMode
	sessTarget string
	warnings   []string

	out      io.Writer
	errw     io.Writer
	provider func(llm.Model) (llm.Provider, error)
}

// runHeadless drives one turn with no terminal and returns the process exit code.
func runHeadless(o headlessOptions) int {
	started := time.Now()
	out, errw := o.out, o.errw
	if out == nil {
		out = os.Stdout
	}
	if errw == nil {
		errw = os.Stderr
	}
	for _, w := range o.warnings {
		_, _ = fmt.Fprintln(errw, "ajent:", w)
	}
	if o.active.ID == "" {
		_, _ = fmt.Fprintln(errw, "ajent: no model configured; set one with -m or in config.json")
		return exitUsage
	}

	var drain headSink
	if o.flags.output == outputJSON {
		drain = newJSONSink(out)
	} else {
		drain = newTextSink(out, errw)
	}
	notify := func(msg string, level agent.Level) {
		_, _ = fmt.Fprintf(errw, "ajent: %s: %s\n", levelName(level), msg)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()

	providerFor := o.provider
	if providerFor == nil {
		providerFor = llm.NewProviders(o.reg).ProviderFor
	}
	st := &agent.State{
		Model:     o.active,
		Reasoning: reasoningFrom(o.set.Settings().Reasoning, o.active),
		Tokens:    tokens.New(o.active),
	}

	// recording keeps -p composable: a follow-up --continue rejoins this transcript.
	rec := newSession(nil, o.sessMode, o.sessTarget, o.active.Key())
	if rec == nil {
		notify("session recording disabled", agent.LevelWarn)
	}

	// ask_user has nobody to ask, so it is left without an Ask func and excluded
	// from every scope below.
	toolsReg, terr := tools.Builtins(tools.Options{SessionID: cwdOrDot()})
	if terr != nil {
		_, _ = fmt.Fprintln(errw, "ajent:", terr)
		return exitUsage
	}

	env := agent.DetectEnvironment()
	globalDir, _ := config.Home()
	proj, perr := agent.LoadProjectInstructions(globalDir, env.Cwd)
	if perr != nil {
		notify("could not read AGENTS.md: "+perr.Error(), agent.LevelWarn)
	}

	var stats *statsSink
	if o.flags.stats {
		stats = newStatsSink()
	}

	var comp *compactor
	opts := agent.Options{
		Sinks:               []agent.Sink{drain},
		Env:                 env,
		ProjectInstructions: proj,
		Tools:               toolsReg,
		Provider:            providerFor,
		Compact: func(ctx context.Context, reason agent.CompactReason) (bool, error) {
			if comp == nil {
				return false, nil // recording is off; nothing to compact
			}
			return comp.run(ctx, reason, "")
		},
		MaxSteps:  o.set.Settings().Agent.MaxSteps,
		SessionID: sessionHint(rec),
	}
	if rec != nil {
		opts.Sinks = []agent.Sink{rec.rec.Sink(drain)}
		opts.OnMessage = []func(agent.MessageInfo){rec.rec.Message}
		_, _, warns := rec.restoreState(o.set, o.reg, st, toolsReg)
		for _, w := range warns {
			notify("resume: "+w, agent.LevelWarn)
		}
	}

	var ag *agent.Agent
	sag := subagent.New(subagent.Options{
		Provider:            providerFor,
		Model:               func() llm.Model { return resolveSubAgentModel(o.set, o.reg, st) },
		Reasoning:           func() llm.ReasoningConfig { return st.Reasoning },
		Parent:              func() *tokens.Accounting { return st.Tokens },
		Tools:               toolsReg,
		Env:                 env,
		ProjectInstructions: proj,
		Notice:              func(msg string) { notify(msg, agent.LevelInfo) },
		Deliver: func(in agent.Input) bool {
			if ag == nil || !ag.Running() {
				return false
			}
			return ag.Steer(in)
		},
		MaxConcurrent: o.set.Settings().Subagent.MaxConcurrent,
	})
	defer sag.Close()
	for _, t := range sag.Tools() {
		toolsReg.RegisterFrom(tools.SourceBuiltin, t, true)
	}
	toolsReg.MarkReadOnly([]string{"agent_start", "agent_poll", "agent_list"})
	if stats != nil { // appended after the recorder settles the drain list
		opts.Sinks = append(opts.Sinks, stats)
	}
	opts.Sinks = append(opts.Sinks, subagentSink{mgr: sag})
	// headless runs have no queued prompts; the boundary hook serves completions
	// alone, deciding membership when the message lands so polls are never duplicated
	opts.OnBoundary = sag.Boundary

	servers, mwarns, merr := mcp.LoadConfig(cwdOrDot())
	if merr != nil {
		notify("mcp: "+merr.Error(), agent.LevelWarn)
	}
	for _, w := range mwarns {
		notify("mcp: "+w, agent.LevelWarn)
	}
	if merr == nil {
		mgr := mcp.New(servers, mcp.Options{
			Registrar: registryAdapter{toolsReg},
			Workspace: cwdOrDot(),
			Restore:   st.Tools,
			Notice:    func(msg string, warn bool) { notify("mcp: "+msg, agentLevelOf(warn)) },
		})
		defer mgr.Close()
		mgr.LoadOnFirstMessage(ctx) // every remote tool must exist before the scope is applied
	}

	// the scope is applied last, once every tool the run could offer is registered
	toolsReg.SetEnabled(headlessTools(toolsReg, o.flags.scope(), o.flags.allowTools, o.flags.denyTools))

	// allow-all with the configured deny list: an operator's explicit gate still
	// holds, and nothing else can prompt a human who is not there.
	barrier := permit.NewBarrier(toolsReg.ReadOnly)
	barrier.SetMode(permit.ModeAllowAll)
	barrier.SetDeniedCommands(o.set.Settings().Permissions.DeniedCommands)
	barrier.SetNotice(func(msg string) { notify(msg, agent.LevelInfo) })
	toolsReg.AddGuard(barrier.Guard())
	toolsReg.SetAsker(barrier.Asker())

	ag = agent.New(st, opts)
	// a resumed ledger carries no base of its own, and buildRequest reads Used for
	// MaxOutputFor before stream() seeds one. A one-shot's tool set is fixed by its
	// flags, so the block is committed from the start.
	st.Tokens.SetBase(ag.BaseEstimate(true))

	if rec != nil {
		comp = &compactor{
			rec: rec, st: st, ag: ag, reg: o.reg,
			sink:        opts.Sinks[0],
			notify:      notify,
			providerFor: providerFor,
			cfg:         func() config.Compaction { return o.set.Settings().Compaction },
		}
	}

	// expand @ references like the pump does, once the scope has settled
	expander := refs.NewExpander(toolsReg, opts.Sinks[0], tools.PathPolicy{Cwd: cwdOrDot()})
	expander.Seed(st.Messages) // --continue reopens a transcript that holds ref ids
	expanded := expander.Expand(o.flags.prompt)
	for _, n := range expanded.Notices {
		notify(n, agent.LevelWarn)
	}
	err := ag.Prompt(ctx, agent.Input{Text: expanded.Text, After: expanded.Run, Injected: true})
	answer := llm.FinalAnswer(st.Messages)
	res := drain.result()
	status, code := headlessOutcome(err, res, answer)
	if status != statusOK {
		// text output prints nothing without an answer, so say why on stderr
		_, _ = fmt.Fprintln(errw, "ajent:", outcomeReason(err, res))
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
		return statusError, exitTurn
	case res.Stop == llm.StopAborted:
		return statusEmpty, exitTurn
	case answer == "":
		return statusEmpty, exitTurn
	default:
		return statusOK, exitOK
	}
}

// headlessTools returns the tool names to enable for scope, then applies the
// allow and deny adjustments. Built-in names follow the scope regardless of
// tools.enabled; every other source keeps its registered state, so a server
// disabled in mcp.json stays off.
func headlessTools(reg *tools.Registry, scope toolScope, allow, deny []string) []string {
	inScope := func(name string) bool {
		if name == "ask_user" { // no human to answer a question headless
			return false
		}
		switch scope {
		case scopeAllowAll:
			return true
		case scopeReadOnly:
			return slices.Contains(tools.ReadOnlyBuiltins, name) || reg.ReadOnly(name)
		default:
			return name != "bash"
		}
	}

	builtins := reg.AllNames(tools.SourceBuiltin)
	names := bulk.SliceFilter(inScope, builtins)

	// enabled non-builtins keep their state, narrowed to read-only under that scope
	builtinSet := bulk.SliceToSet(builtins)
	for _, name := range reg.Names() {
		if _, ok := builtinSet[name]; ok {
			continue
		} else if scope == scopeReadOnly && !reg.ReadOnly(name) {
			continue
		}
		names = append(names, name)
	}

	names = append(names, allow...)
	denied := bulk.SliceToSet(deny)
	names = bulk.SliceFilterInPlace(func(n string) bool {
		_, ok := denied[n]
		return !ok
	}, names)
	return slices.Compact(slices.Sorted(slices.Values(names)))
}

// agentLevelOf maps a warn flag onto a notice level.
func agentLevelOf(warn bool) agent.Level {
	if warn {
		return agent.LevelWarn
	}
	return agent.LevelInfo
}
