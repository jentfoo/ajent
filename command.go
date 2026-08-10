package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

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
// truth for the active model, which is the seam the agent loop takes over.
func handleCommand(ui *tui.UI, reg *llm.Registry, cmd, arg string) {
	switch cmd {
	case "model":
		selectModel(ui, reg, arg)
	default:
		ui.Notify("unknown command /"+cmd, tui.LevelWarn)
	}
}

// selectModel resolves arg, or opens the picker when it is empty.
func selectModel(ui *tui.UI, reg *llm.Registry, arg string) {
	models := reg.Models()
	if len(models) == 0 {
		ui.Notify("no models configured, add some to ~/.ajent/"+llm.ModelsFileName, tui.LevelWarn)
		return
	}
	if arg != "" {
		m, err := reg.Resolve(arg)
		if err != nil {
			reportResolveError(ui, arg, err)
			return
		}
		applyModel(ui, reg, m)
		return
	}

	items := make([]tui.PickItem, len(models))
	active := reg.Active().Key()
	var initial int
	for i, m := range models {
		if m.Key() == active {
			initial = i
		}
		items[i] = tui.PickItem{
			Label:  m.Key(),
			Detail: modelDetail(m),
			Terms:  append([]string{m.Name}, m.Aliases...),
		}
	}

	i, err := ui.PickContext(context.Background(), "Model", items,
		tui.PickOptions{Placeholder: "filter", Initial: initial})
	if err != nil {
		return // cancelled, nothing to report
	}
	applyModel(ui, reg, models[i])
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

// applyModel makes m active and reflects it in the status line.
func applyModel(ui *tui.UI, reg *llm.Registry, m llm.Model) {
	reg.SetActive(m)
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
