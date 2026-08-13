package command

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// modelCommand resolves arg by name, or opens the picker when empty. The
// registry stays the single source of truth; Console.SetModel reflects the
// choice in the status line, agent state and session entry.
func modelCommand(_ context.Context, arg string, c Console) error {
	models := c.Models().Models()
	if len(models) == 0 {
		c.Notify("no models configured; add some to ~/.ajent/"+llm.ModelsFileName, levelWarn)
		return nil
	}
	var m llm.Model
	if arg != "" {
		target, err := c.Models().Resolve(arg)
		if err != nil {
			reportResolveError(c, arg, err)
			return nil
		}
		m = target
	} else {
		items := make([]tui.PickItem, len(models))
		activeKey := c.Models().Active().Key()
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
		picked, err := c.Pick(context.Background(), "Model", items,
			tui.PickOptions{Placeholder: "filter", Initial: initial})
		if err != nil {
			return nil // cancelled, nothing to report
		}
		m = models[picked]
	}
	c.SetModel(m)
	return nil
}

// modelCompletion returns model keys and aliases for /model argument completion.
func modelCompletion(c Console) func(prefix string) []string {
	return func(prefix string) []string {
		models := c.Models().Models()
		out := make([]string, 0, len(models)*2)
		for _, m := range models {
			out = append(out, m.Key())
			out = append(out, m.Aliases...)
		}
		return filterPrefix(out, prefix)
	}
}

// modelDetail is the dim trailing text on a picker row.
func modelDetail(m llm.Model) string {
	parts := []string{m.Display()}
	if m.ContextWindow > 0 {
		parts = append(parts, tui.FormatTokens(m.ContextWindow))
	}
	if m.Caps.Reasoning {
		parts = append(parts, "reasoning")
	}
	return strings.Join(parts, " · ")
}

// reportResolveError explains why a name did not select a model.
func reportResolveError(c Console, arg string, err error) {
	var ambiguous *llm.ErrAmbiguousModel
	if errors.As(err, &ambiguous) {
		c.Notify(arg+" matches "+strings.Join(ambiguous.Candidates, ", "), levelWarn)
		return
	}
	c.Notify("no model matches "+arg, levelWarn)
}

// reasoningCommand sets the session reasoning level. With no argument it reports
// the current choice; with a level it sets it and persists through the console.
func reasoningCommand(_ context.Context, arg string, c Console) error {
	active := c.Models().Active()
	supported := llm.LevelsFor(active)
	var lvl llm.Level

	if strings.TrimSpace(arg) == "" {
		items := make([]tui.PickItem, len(supported))
		for i, l := range supported {
			name := l.String()
			marker := "  "
			if c.State().Reasoning.Level == l {
				marker = "* "
			}
			items[i] = tui.PickItem{Label: marker + name, Terms: []string{name}}
		}
		picked, err := c.Pick(context.Background(), "Reasoning", items,
			tui.PickOptions{Placeholder: "filter"})
		if err != nil {
			return nil // cancelled
		}
		lvl = supported[picked]
	} else {
		var ok bool
		lvl, ok = llm.ParseLevel(arg)
		if !ok {
			c.Notify("unknown reasoning level "+arg+"; use "+levelsList(supported), levelWarn)
			return nil
		}
		// never store a level the encoder will silently drop: snap to what is sent
		if !slices.Contains(supported, lvl) {
			c.Notify(lvl.String()+" unsupported for this model; using "+llm.ClampLevel(active, lvl).String(),
				levelWarn)
			lvl = llm.ClampLevel(active, lvl)
		}
	}

	c.SetReasoning(llm.ReasoningConfig{
		Level:  lvl,
		Retain: c.State().Reasoning.Retain, // keep the current retention policy
		Show:   true,
	})
	return nil
}

// levelsList joins level names for a supported-level notice.
func levelsList(levels []llm.Level) string {
	names := make([]string, len(levels))
	for i, l := range levels {
		names[i] = l.String()
	}
	return strings.Join(names, "|")
}
