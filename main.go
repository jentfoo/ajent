package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
	tuisink "github.com/jentfoo/ajent/pkg/tui/sink"
)

// defaultMaxTokens is the context window shown when no model reports one.
const defaultMaxTokens = 200_000

func main() {
	var modelFlag string
	flag.StringVar(&modelFlag, "m", "", "initial model to use")
	flag.StringVar(&modelFlag, "model", "", "initial model to use")
	render := flag.String("render", "auto",
		"paint mode: auto, inline (terminal scrollback, unsupported under tmux or screen), "+
			"alt (own scrollback), plain")
	flag.Parse()

	mode, ok := tui.ParseMode(*render)
	if !ok {
		fmt.Fprintf(os.Stderr, "ajent: unknown render mode %q\n", *render)
		os.Exit(2)
	}

	file, warnings, err := llm.LoadUserFile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	reg, regWarnings := llm.NewRegistry(file, llm.LoadUserCache(), llm.RegistryOptions{})
	warnings = append(warnings, regWarnings...)

	active := reg.Active()
	if modelFlag != "" {
		m, err := reg.Resolve(modelFlag)
		if err == nil {
			reg.SetActive(m)
			active = m
		} else {
			warnings = append(warnings, "no model matches "+modelFlag)
		}
	}

	maxTokens := defaultMaxTokens
	var label string
	if active.ID != "" {
		label = active.Key()
		if active.ContextWindow > 0 {
			maxTokens = active.ContextWindow
		}
	}

	ui, err := tui.New(tui.Options{
		Mode:      mode,
		Model:     label,
		MaxTokens: maxTokens,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "ajent:", err)
		os.Exit(1)
	}
	defer ui.Close()

	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}
	go refreshModels(ui, reg)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		ui.Close()
		os.Exit(0)
	}()

	driver(ui, reg, active, flag.Args())
}

// driver runs the real agent loop: it builds an Agent over the registry and
// drives turns from submitted messages, steering mid-turn input into the running
// turn rather than starting a second one.
func driver(ui *tui.UI, reg *llm.Registry, active llm.Model, args []string) {
	providers := llm.NewProviders(reg)
	st := &agent.State{
		Model:     active,
		Reasoning: defaultReasoning(),
	}
	sink := tuisink.New(ui)
	ag := agent.New(st, agent.Options{
		Sink: sink,
		Env:  agent.DetectEnvironment(),
		Provider: func(m llm.Model) (llm.Provider, error) {
			return providers.ProviderFor(m)
		},
	})

	if active.ID == "" {
		ui.Notify("no model configured; use /model to pick one", tui.LevelWarn)
	}

	quit := watchControls(ui, ag)

	var seed agent.Input
	if len(args) > 0 {
		seed = agent.Input{Text: strings.Join(args, " ")}
		startTurn(ui, ag, seed.Text)
	}

	for {
		select {
		case msg, ok := <-ui.Messages():
			if !ok {
				return // UI closed
			}
			if cmd, arg, ok := slashCommand(msg); ok {
				handleCommand(ui, reg, st, cmd, arg)
				continue
			}
			if ag.Steer(agent.Input{Text: msg}) {
				continue // queued into the running turn at its next boundary
			}
			startTurn(ui, ag, msg)
		case <-quit:
			return
		}
	}
}

// startTurn echoes a submitted message and runs it to completion in the
// background so the main loop keeps receiving input while the model streams.
func startTurn(ui *tui.UI, ag *agent.Agent, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	ui.UserEcho(text)
	go func() { _ = ag.Prompt(context.Background(), agent.Input{Text: text}) }()
}

// defaultReasoning is the reasoning config for a fresh session; off until a
// /reasoning command lands.
func defaultReasoning() llm.ReasoningConfig {
	return llm.ReasoningConfig{}
}

// doublePressWindow is how long a second Ctrl+C on an idle editor must arrive
// within to quit, so a single stray interrupt never kills the app.
const doublePressWindow = 2 * time.Second

// watchControls interprets out-of-band keys: Esc and Ctrl+C interrupt a running
// turn, while Ctrl+D or a double Ctrl+C on an idle empty editor quits. Closing
// quit signals driver to return, which lets main's deferred ui.Close restore the
// terminal.
func watchControls(ui *tui.UI, ag *agent.Agent) <-chan struct{} {
	quit := make(chan struct{})
	go func() {
		var lastInt time.Time
		for c := range ui.Controls() {
			switch c {
			case tui.ControlEscape:
				if ag.Running() {
					ag.Interrupt()
				}
			case tui.ControlInterrupt:
				if ag.Running() {
					ag.Interrupt()
					continue
				}
				now := time.Now()
				if now.Sub(lastInt) < doublePressWindow {
					close(quit)
					return
				}
				lastInt = now
				ui.SetStatusSegment("hint", "ctrl+c again to quit")
			case tui.ControlEOF:
				if ag.Running() {
					continue // ignored while a turn streams, per the key table
				}
				close(quit) // emitted only on an empty editor: deliberate exit
				return
			}
		}
	}()
	return quit
}

// discoverTimeout bounds a background model discovery pass, so an unreachable
// endpoint cannot leave it running for the life of the session.
const discoverTimeout = 30 * time.Second

// refreshModels updates the model list from the providers that can list their
// own, in the background so startup never waits on the network. A failure is
// only ever a notice: whatever was cached still works.
func refreshModels(ui *tui.UI, reg *llm.Registry) {
	ctx, cancel := context.WithTimeout(context.Background(), discoverTimeout)
	defer cancel()

	before := len(reg.Models())
	cache, warnings := reg.Refresh(ctx, llm.DiscoverOptions{})
	for _, w := range warnings {
		ui.Notify(w, tui.LevelWarn)
	}
	if err := llm.SaveUserCache(cache); err != nil {
		ui.Notify("could not save the model cache: "+err.Error(), tui.LevelWarn)
	}
	if added := len(reg.Models()) - before; added > 0 {
		ui.NotifyKeyed("models", "discovered "+strconv.Itoa(added)+" more models", tui.LevelInfo)
	}
}

// slashCommand splits a submitted line into a command and its argument,
// reporting whether it was one at all.
func slashCommand(msg string) (cmd, arg string, ok bool) {
	trimmed := strings.TrimSpace(msg)
	if !strings.HasPrefix(trimmed, "/") {
		return "", "", false
	}
	cmd, arg, _ = strings.Cut(trimmed[1:], " ")
	return strings.ToLower(cmd), strings.TrimSpace(arg), cmd != ""
}

// handleCommand runs a slash command. The registry stays the single source of
// truth for the active model; st.Model follows it so later turns use the new one.
func handleCommand(ui *tui.UI, reg *llm.Registry, st *agent.State, cmd, arg string) {
	switch cmd {
	case "model":
		selectModel(ui, reg, st, arg)
	default:
		ui.Notify("unknown command /"+cmd, tui.LevelWarn)
	}
}

// selectModel resolves arg, or opens the picker when it is empty.
func selectModel(ui *tui.UI, reg *llm.Registry, st *agent.State, arg string) {
	models := reg.Models()
	if len(models) == 0 {
		ui.Notify("no models configured; add some to ~/.ajent/"+llm.ModelsFileName, tui.LevelWarn)
		return
	}
	var m llm.Model
	if arg != "" {
		target, err := reg.Resolve(arg)
		if err != nil {
			reportResolveError(ui, arg, err)
			return
		}
		m = target
	} else {
		items := make([]tui.PickItem, len(models))
		activeKey := reg.Active().Key()
		var initial int
		for i, mod := range models {
			if mod.Key() == activeKey {
				initial = i
			}
			items[i] = tui.PickItem{
				Label:  mod.Key(),
				Detail: modelDetail(mod),
				Terms:  append([]string{mod.Name}, mod.Aliases...),
			}
		}
		picked, err := ui.PickContext(context.Background(), "Model", items,
			tui.PickOptions{Placeholder: "filter", Initial: initial})
		if err != nil {
			return // cancelled, nothing to report
		}
		m = models[picked]
	}
	applyModel(ui, reg, st, m)
}

// modelDetail is the dim trailing text on a picker row.
func modelDetail(m llm.Model) string {
	parts := []string{m.Display()}
	if m.ContextWindow > 0 {
		parts = append(parts, formatTokens(m.ContextWindow))
	}
	if m.Caps.Reasoning != llm.ReasoningNone {
		parts = append(parts, "reasoning")
	}
	return strings.Join(parts, " · ")
}

// applyModel makes m active and reflects it in the status line and the agent's
// state so subsequent turns use it.
func applyModel(ui *tui.UI, reg *llm.Registry, st *agent.State, m llm.Model) {
	reg.SetActive(m)
	st.Model = m
	ui.SetModel(m.Key(), m.ContextWindow)
	ui.Notify("model: "+m.Key(), tui.LevelInfo)
}

// reportResolveError explains why a name did not select a model.
func reportResolveError(ui *tui.UI, arg string, err error) {
	var ambiguous *llm.ErrAmbiguousModel
	if errors.As(err, &ambiguous) {
		ui.Notify(arg+" matches "+strings.Join(ambiguous.Candidates, ", "), tui.LevelWarn)
		return
	}
	ui.Notify("no model matches "+arg, tui.LevelWarn)
}

// formatTokens abbreviates a context window, such as 200k or 1.2M.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64), ".0") + "M"
	case n >= 1_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1_000, 'f', 1, 64), ".0") + "k"
	default:
		return strconv.Itoa(n)
	}
}
