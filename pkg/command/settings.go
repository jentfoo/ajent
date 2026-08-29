package command

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// settingsRow is one /settings menu entry: its display, and the editor that
// changes it. edit returns the config keys/values a change persists so the save
// prompt can copy them to a file layer.
type settingsRow struct {
	name   string
	render func(c Console) (label, detail string)
	edit   func(ctx context.Context, c Console) ([]settingChange, error)
}

// settingChange is one config key/value an editor produced for persistence. An
// edit may yield several (e.g. auto-compaction's toggle and threshold).
type settingChange struct {
	key   string
	value any
}

// settingsCommand shows or edits configuration. No section opens the full menu;
// a section name jumps straight to that row's editor.
func settingsCommand(_ context.Context, arg string, c Console) error {
	return runSettings(c, strings.TrimSpace(arg))
}

// settingsCompletion offers section names for /settings <section>.
func settingsCompletion(c Console) func(prefix string) []string {
	return func(prefix string) []string {
		out := make([]string, 0, len(allRows()))
		for _, r := range allRows() {
			out = append(out, r.name)
		}
		return filterPrefix(out, prefix)
	}
}

// runSettings drives the menu loop. A section jumps to one row then returns; an
// empty section reopens after every edit until cancelled.
func runSettings(c Console, section string) error {
	if section != "" {
		for i := range allRows() {
			r := &allRows()[i]
			if strings.EqualFold(r.name, section) || (section == "reasoning" && r.name == "Reasoning") {
				return editRow(context.Background(), c, r)
			}
		}
		c.Notify("no settings section "+section+"; try /settings", levelWarn)
		return nil
	}

	last := 0
	for {
		rows := allRows()
		items := make([]tui.PickItem, len(rows))
		for i := range rows {
			label, detail := rows[i].render(c)
			items[i] = tui.PickItem{Label: label, Detail: detail}
		}
		picked, err := c.Pick(context.Background(), "Settings", items,
			tui.PickOptions{Initial: last})
		if err != nil {
			return nil // cancelled
		}
		err = editRow(context.Background(), c, &rows[picked])
		if errorsIsCancelled(err) {
			continue // Esc leaves the row; reopen the menu on it
		} else if err != nil {
			c.Notify("settings: "+err.Error(), levelWarn)
		}
		last = picked
	}
}

// editRow runs one editor and offers to persist its change.
func editRow(ctx context.Context, c Console, r *settingsRow) error {
	changes, err := r.edit(ctx, c)
	if err != nil || len(changes) == 0 {
		return err // nothing changed or a row with no persistent key
	}
	savePrompt(c, changes...)
	return nil
}

// savePrompt asks where a just-applied session override should persist. The
// editor already SetSession; this only copies to a file layer when chosen.
func savePrompt(c Console, changes ...settingChange) {
	idx, err := c.Select(context.Background(), "Save change",
		[]tui.Option{
			{Label: "this session only"},
			{Label: "save to user config"},
			{Label: "save to project config"},
		})
	if err != nil || idx == 0 {
		return // Esc or Enter on the default keeps it for this session
	}
	var layer string
	switch idx {
	case 1:
		layer = "user"
	case 2:
		layer = "project"
	default:
		return
	}
	keys := make([]string, len(changes))
	for i, ch := range changes {
		if serr := c.SaveSetting(layer, ch.key, ch.value); serr != nil {
			c.Notify("could not save "+ch.key+": "+serr.Error(), levelWarn)
			return
		}
		keys[i] = ch.key
	}
	c.Notify(fmt.Sprintf("%s saved to %s config", strings.Join(keys, ", "), layer), levelInfo)
}

// enumRow builds a row editing a string key from a fixed set of values.
func enumRow(name, key string, values []string) settingsRow {
	edit := func(_ context.Context, c Console) ([]settingChange, error) {
		opts := make([]tui.Option, len(values))
		current, _, _ := c.Settings().Explain(key)
		var cur string
		_ = json.Unmarshal(current, &cur)
		for i, v := range values {
			marker := "  "
			if v == cur {
				marker = "* "
			}
			opts[i] = tui.Option{Label: marker + v}
		}
		idx, err := c.Select(context.Background(), name, opts)
		if err != nil {
			return nil, err
		}
		sel := values[idx]
		_ = c.SetSessionSetting(key, sel)
		return []settingChange{{key: key, value: sel}}, nil
	}
	return settingsRow{name: name,
		render: func(c Console) (string, string) { return name, detailOrDefault(c, key) },
		edit:   edit}
}

// intRow builds a row editing an integer key within [min,max]. The edit reads
// the current value, prompts for a replacement and validates before persisting,
// so it fits numeric fields that enumRow's string storage cannot unmarshal into.
func intRow(name, key string, min, max int) settingsRow {
	edit := func(_ context.Context, c Console) ([]settingChange, error) {
		current, _, _ := c.Settings().Explain(key)
		var cur int
		_ = json.Unmarshal(current, &cur)
		in, err := c.Input(context.Background(), name, strconv.Itoa(cur))
		if err != nil {
			return nil, err
		}
		n, perr := strconv.Atoi(strings.TrimSpace(in))
		if perr != nil || n < min || n > max {
			c.Notify(fmt.Sprintf("%s must be between %d and %d", name, min, max), levelWarn)
			return nil, nil
		}
		_ = c.SetSessionSetting(key, n)
		return []settingChange{{key: key, value: n}}, nil
	}
	return settingsRow{name: name,
		render: func(c Console) (string, string) { return name, detailOrDefault(c, key) },
		edit:   edit}
}

// floatRow builds a row editing a fractional key within [min,max], the numeric
// counterpart of intRow for settings stored as a fraction.
func floatRow(name, key string, min, max float64) settingsRow {
	edit := func(_ context.Context, c Console) ([]settingChange, error) {
		current, _, _ := c.Settings().Explain(key)
		var cur float64
		_ = json.Unmarshal(current, &cur)
		in, err := c.Input(context.Background(), name, strconv.FormatFloat(cur, 'g', -1, 64))
		if err != nil {
			return nil, err
		}
		f, perr := strconv.ParseFloat(strings.TrimSpace(in), 64)
		if perr != nil || f < min || f > max {
			c.Notify(fmt.Sprintf("%s must be between %g and %g", name, min, max), levelWarn)
			return nil, nil
		}
		_ = c.SetSessionSetting(key, f)
		return []settingChange{{key: key, value: f}}, nil
	}
	return settingsRow{name: name,
		render: func(c Console) (string, string) { return name, detailOrDefault(c, key) },
		edit:   edit}
}

// modelRow builds a row editing a model reference key through the model picker.
func modelRow(name, key string) settingsRow {
	render := func(c Console) (string, string) { return name, detailOrDefault(c, key) }
	edit := func(ctx context.Context, c Console) ([]settingChange, error) {
		raw, _, _ := c.Settings().Explain(key)
		var cur string
		_ = json.Unmarshal(raw, &cur)
		m, err := PickModel(ctx, c, "Model", cur, tui.PickOptions{})
		if err != nil {
			return nil, err
		}
		_ = c.SetSessionSetting(key, m.Key())
		return []settingChange{{key: key, value: m.Key()}}, nil
	}
	return settingsRow{name: name, render: render, edit: edit}
}

// allRows returns the settings menu in display order.
func allRows() []settingsRow {
	rows := []settingsRow{
		{name: "Model", render: rowModel, edit: editModel},
		{name: "Reasoning", render: rowReasoning, edit: editReasoning},
		{name: "Reasoning retention", render: rowRetention, edit: editRetention},
		{name: "Show thinking", render: rowThinking, edit: editThinking},
		{name: "Tools", render: rowTools, edit: editTools},
		{name: "Auto-compaction", render: rowCompaction, edit: editCompaction},
		intRow("Compaction verbatim steps", "compaction.minSteps", 1, 8),
		floatRow("Compaction verbatim size", "compaction.verbatimFraction", 0.01, 0.5),
		enumRow("Permissions mode", "permissions.mode", []string{"allow-all", "allow-read", "auto", "auto+mcp", "auto+write", "block-all"}),
		modelRow("Sub-agent model", "subagent.model"),
		intRow("Sub-agent concurrency", "subagent.maxConcurrent", 1, 64),
		themeRow(),
		{name: "Tool limits",
			render: func(_ Console) (string, string) { return "Tool limits", "edit per-tool output bounds" },
			edit:   func(_ context.Context, c Console) ([]settingChange, error) { return editLimits(c) }},
	}
	return rows
}

// detail renders a key's raw value with its source layer.
func detail(c Console, key string) (string, bool) {
	v, src, ok := c.Settings().Explain(key)
	if !ok || len(v) == 0 {
		return "", false
	}
	var out any
	_ = json.Unmarshal(v, &out)
	s := fmt.Sprintf("%v", out)
	switch s {
	case "true":
		s = "on"
	case "false":
		s = "off"
	}
	return fmt.Sprintf("%s  (%s)", s, orDefault(src)), true
}

func rowModel(c Console) (string, string) {
	d, ok := detail(c, "model")
	if !ok {
		return "Model", c.Models().Active().Key() + "  (" + sessionOrFlag(c) + ")"
	}
	return "Model", d
}

func rowReasoning(c Console) (string, string) {
	return "Reasoning", detailOrDefault(c, "reasoning.level")
}
func rowRetention(c Console) (string, string) {
	return "Reasoning retention", detailOrDefault(c, "reasoning.retain")
}
func rowThinking(c Console) (string, string) {
	return "Show thinking", detailOrDefault(c, "reasoning.show")
}

func rowTools(c Console) (string, string) {
	n := len(c.Settings().Settings().Tools.Enabled)
	total := 0
	if tr := c.Tools(); tr != nil {
		total = len(tr.All())
	}
	return "Tools", fmt.Sprintf("%d of %d enabled  (%s)", n, total, sessionOrFlag(c))
}

func rowCompaction(c Console) (string, string) {
	auto, asrc, _ := c.Settings().Explain("compaction.auto")
	pct, _, _ := c.Settings().Explain("compaction.threshold")
	var f float64
	_ = json.Unmarshal(pct, &f)
	thr := fmt.Sprintf("%g tokens", f) // an absolute token count when >= 1
	if f < 1 {
		thr = fmt.Sprintf("%.0f%%", f*100)
	}
	return "Auto-compaction",
		fmt.Sprintf("%s, %s  (%s)", boolWord(auto), thr, orDefault(asrc))
}

// editModel delegates to the model picker.
func editModel(ctx context.Context, c Console) ([]settingChange, error) {
	err := modelCommand(ctx, "", c)
	if err != nil {
		return nil, err
	}
	key := c.Models().Active().Key()
	_ = c.SetSessionSetting("model", key)
	return []settingChange{{key: "model", value: key}}, nil
}

// editReasoning delegates to the reasoning picker.
func editReasoning(_ context.Context, c Console) ([]settingChange, error) {
	err := reasoningCommand(context.Background(), "", c)
	if err != nil || c.State() == nil {
		return nil, err
	}
	// persist the level as a dotted leaf; SetReasoning already updated state.
	level := c.State().Reasoning.Level.String()
	_ = c.SetSessionSetting("reasoning.level", level)
	return []settingChange{{key: "reasoning.level", value: level}}, nil
}

// editRetention lets the user pick a reasoning retention policy.
func editRetention(_ context.Context, c Console) ([]settingChange, error) {
	policies := []string{"none", "lastTurn", "wholeTurn", "all"}
	current := llm.RetainWholeTurn
	if p, ok := llm.ParseRetain(c.State().Reasoning.Retain.String()); ok {
		current = p
	}
	opts := make([]tui.Option, len(policies))
	for i, p := range policies {
		marker := "  "
		if p == current.String() {
			marker = "* "
		}
		opts[i] = tui.Option{Label: marker + p}
	}
	idx, err := c.Select(context.Background(), "Reasoning retention", opts)
	if err != nil {
		return nil, err
	}
	retain, ok := llm.ParseRetain(policies[idx])
	if !ok {
		return nil, fmt.Errorf("unknown policy %q", policies[idx])
	}
	c.SetReasoning(llm.ReasoningConfig{
		Level:  c.State().Reasoning.Level,
		Budget: c.State().Reasoning.Budget,
		Retain: retain,
		Show:   c.State().Reasoning.Show,
	})
	// a text leaf survives even when the policy is "none" (zero value).
	_ = c.SetSessionSetting("reasoning.retain", retain.String())
	return []settingChange{{key: "reasoning.retain", value: retain.String()}}, nil
}

// editThinking flips whether thinking streams to the UI.
func editThinking(_ context.Context, c Console) ([]settingChange, error) {
	on, err := c.Confirm(context.Background(), "Stream thinking to the UI?")
	if err != nil {
		return nil, err
	}
	c.SetReasoning(llm.ReasoningConfig{
		Level:  c.State().Reasoning.Level,
		Budget: c.State().Reasoning.Budget,
		Retain: c.State().Reasoning.Retain,
		Show:   on,
	})
	_ = c.SetSessionSetting("reasoning.show", on)
	return []settingChange{{key: "reasoning.show", value: on}}, nil
}

// editTools delegates to the tools picker.
func editTools(ctx context.Context, c Console) ([]settingChange, error) {
	err := toolsCommand(ctx, "", c)
	if err != nil || c.Tools() == nil {
		return nil, err
	}
	names := toolNames(c)
	_ = c.SetSessionSetting("tools.enabled", names)
	return []settingChange{{key: "tools.enabled", value: names}}, nil
}

// editCompaction toggles auto-compaction and sets its threshold. The input is
// gathered and validated before anything applies: valid values are a fraction in
// (0,1) of the window or an absolute token count >= 1; anything else aborts the
// whole edit with no session settings recorded.
func editCompaction(_ context.Context, c Console) ([]settingChange, error) {
	on, err := c.Confirm(context.Background(), "Enable automatic compaction?")
	if err != nil {
		return nil, err
	}

	threshold := 0.8
	pct, _, _ := c.Settings().Explain("compaction.threshold")
	_ = json.Unmarshal(pct, &threshold)

	if on {
		in, ierr := c.Input(context.Background(), "Threshold (fraction of window or absolute tokens)", fmt.Sprintf("%g", threshold))
		if ierr != nil {
			return nil, ierr
		}
		// Enter on the placeholder keeps the current threshold; anything else must be a positive number.
		if in != "" {
			f, perr := strconv.ParseFloat(strings.TrimSpace(in), 64)
			if perr != nil || f <= 0 {
				c.Notify("threshold must be a number greater than zero", levelWarn)
				return nil, nil // abort the whole edit before any setting changes apply
			}
			threshold = f
		}
	}

	_ = c.SetSessionSetting("compaction.auto", on)
	changes := []settingChange{{key: "compaction.auto", value: on}}
	if on {
		// persist the threshold alongside the toggle so a save keeps both.
		_ = c.SetSessionSetting("compaction.threshold", threshold)
		changes = append(changes, settingChange{key: "compaction.threshold", value: threshold})
	}
	return changes, nil
}

// editLimits opens a sub-pick over flattened tool limit dimensions.
func editLimits(c Console) ([]settingChange, error) {
	dims := []struct{ key, label string }{
		{"tools.limits.bash.lines", "bash lines"},
		{"tools.limits.bash.bytes", "bash bytes"},
		{"tools.limits.read.lines", "read lines"},
		{"tools.limits.read.bytes", "read bytes"},
	}
	items := make([]tui.PickItem, len(dims))
	for i, d := range dims {
		v, _, _ := c.Settings().Explain(d.key)
		var n int
		_ = json.Unmarshal(v, &n)
		items[i] = tui.PickItem{Label: d.label, Detail: strconv.Itoa(n)}
	}
	picked, err := c.Pick(context.Background(), "Tool limits", items, tui.PickOptions{})
	if err != nil {
		return nil, err
	}
	in, ierr := c.Input(context.Background(), dims[picked].label, "")
	if ierr != nil {
		return nil, ierr
	}
	n, perr := strconv.Atoi(strings.TrimSpace(in))
	if perr != nil || n <= 0 {
		c.Notify("limit must be a positive integer", levelWarn)
		return nil, nil
	}
	key := dims[picked].key
	_ = c.SetSessionSetting(key, n)
	return []settingChange{{key: key, value: n}}, nil
}

// helpers ----------------------------------------------------------------

// toolNames returns the currently enabled tool names in order.
func toolNames(c Console) []string {
	if c.Tools() == nil {
		return nil
	}
	return slices.Clone(c.Tools().Names())
}

// detailOrDefault renders a row's value and source, or the default layer.
func detailOrDefault(c Console, key string) string {
	d, ok := detail(c, key)
	if !ok {
		return "(default)"
	}
	return d
}

// sessionOrFlag always reports the runtime/session layer.
func sessionOrFlag(_ Console) string { return "session" }

func orDefault(src string) string {
	if src == "" {
		return "default"
	}
	return src
}

func boolWord(raw json.RawMessage) string { return onOffStr(jsonBool(raw)) }
func onOffStr(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// jsonBool decodes a raw JSON boolean.
func jsonBool(raw json.RawMessage) bool {
	var b bool
	_ = json.Unmarshal(raw, &b)
	return b
}

// errorsIsCancelled reports a cancelled interaction.
func errorsIsCancelled(err error) bool { return errors.Is(err, tui.ErrCancelled) }
