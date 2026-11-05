package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Errors SetSetting reports, mapped by the control server onto wire codes.
var (
	// ErrUnknownSetting names a key no /settings row owns.
	ErrUnknownSetting = errors.New("unknown setting")
	// ErrSettingUnavailable wraps the reason one row cannot be edited right now.
	ErrSettingUnavailable = errors.New("setting unavailable")
)

// settingKind is the wire shape one setting's value takes, mirrored in the
// spec table and the parse switch.
const (
	settingKindBool   = "bool"
	settingKindInt    = "int"
	settingKindFloat  = "float"
	settingKindString = "string"
	settingKindModel  = "model"
	settingKindTools  = "tools"
)

// keyModel is the active-model dotted key the model row and editors share.
const keyModel = "model"

// labelModel is the model row's display label.
const labelModel = "Model"

// settingSpec is one /settings row a non-dialog caller may read and apply:
// its dotted key, the value shape it accepts and the apply that installs a
// change with its live effects. The /settings editors and the control socket
// share these applies, so a change recorded from either surface always takes
// effect.
type settingSpec struct {
	key   string
	label string
	kind  string // bool, int, float, string, model or tools
	enums []string
	min   *float64 // inclusive numeric bounds, nil unchecked
	max   *float64
	// positive rejects zero and negatives (a threshold fraction or token count)
	positive bool

	apply   func(c Console, key string, v any) error
	current func(c Console) (json.RawMessage, string) // overrides Explain for live-sourced rows
	off     func(c Console) string                    // why editing is refused now, "" when fine
}

// SettingRow is one /settings entry as the control socket carries it: label,
// dotted key, the resolved value with the layer it came from, and why it
// cannot be edited right now when it cannot.
type SettingRow struct {
	Label    string
	Key      string
	Value    json.RawMessage
	Source   string
	Editable bool
	Reason   string
}

// floatPtr builds one spec bound.
func floatPtr(v float64) *float64 { return &v }

// noModels reports why model-backed rows cannot be edited without a registry.
func noModels(c Console) string {
	if len(c.Models().Models()) == 0 {
		return "no models configured"
	}
	return ""
}

// noToolsRegistered reports why the tools row cannot be edited.
func noToolsRegistered(c Console) string {
	if c.Tools() == nil {
		return "no tools registered"
	}
	return ""
}

// permissionModes is the accepted permissions.mode vocabulary, the set the
// /settings row offers.
var permissionModes = []string{"allow-all", "allow-read", "auto", "auto+write", "block-all"}

// retainPolicies is the accepted reasoning.retain vocabulary.
var retainPolicies = []string{"none", "lastTurn", "wholeTurn", "all"}

// settingSpecs lists every wire-editable row in /settings menu order. Theme
// has no config key and Reset overrides is an action, so neither carries a
// dotted key and neither rides the wire.
func settingSpecs() []settingSpec {
	return []settingSpec{
		{
			key: keyModel, label: labelModel, kind: settingKindModel,
			apply: func(c Console, _ string, v any) error { return applyModelSetting(c, v.(string)) },
			current: func(c Console) (json.RawMessage, string) {
				if raw, src, ok := c.Settings().Explain(keyModel); ok {
					return raw, src
				}
				// unset: the run still sits on a model, the active one
				return mustSettingJSON(c.Models().Active().Key()), "active"
			},
			off: noModels,
		},
		{
			key: "reasoning.level", label: "Reasoning", kind: settingKindString,
			apply: func(c Console, _ string, v any) error { return applyReasoningLevel(c, v.(string)) },
			off:   reasoningUnavailable,
		},
		{
			key: "reasoning.retain", label: "Reasoning retention", kind: settingKindString, enums: retainPolicies,
			apply: func(c Console, _ string, v any) error { return applyReasoningRetain(c, v.(string)) },
			current: func(c Console) (json.RawMessage, string) {
				// the flavor default lives in caps, never in a config layer
				_, src, _ := c.Settings().Explain("reasoning.retain")
				return mustSettingJSON(c.State().Reasoning.Retain.String()), orDefault(src)
			},
		},
		{
			key: "reasoning.hide", label: "Show thinking", kind: settingKindBool,
			apply: func(c Console, _ string, v any) error { return applyReasoningHide(c, v.(bool)) },
		},
		{
			key: "tools.enabled", label: "Tools", kind: settingKindTools,
			apply: func(c Console, _ string, v any) error {
				ApplyToolSet(c, v.([]string))
				return nil
			},
			current: func(c Console) (json.RawMessage, string) {
				_, src, _ := c.Settings().Explain("tools.enabled")
				if reg := c.Tools(); reg != nil {
					return mustSettingJSON(reg.Names()), orDefault(src)
				}
				return nil, orDefault(src)
			},
			off: noToolsRegistered,
		},
		{
			key: "compaction.auto", label: "Auto-compaction", kind: settingKindBool,
			apply: settingApply,
		},
		{
			// any positive number: a window fraction below 1, absolute tokens above
			key: "compaction.threshold", label: "Compaction threshold", kind: settingKindFloat,
			positive: true, apply: settingApply,
		},
		{
			key: "compaction.minSteps", label: "Compaction verbatim steps", kind: settingKindInt,
			min: floatPtr(1), max: floatPtr(8), apply: settingApply,
		},
		{
			key: "compaction.verbatimFraction", label: "Compaction verbatim size", kind: settingKindFloat,
			min: floatPtr(0.01), max: floatPtr(0.5), apply: settingApply,
		},
		{
			// drives the live barrier and records no setting_change by design
			key: "permissions.mode", label: "Permissions mode", kind: settingKindString, enums: permissionModes,
			apply: settingApply,
		},
		{
			key: "subagent.model", label: "Sub-agent model", kind: settingKindModel,
			apply: func(c Console, _ string, v any) error { return applySubagentModel(c, v.(string)) },
			off:   noModels,
		},
		{
			key: "subagent.maxConcurrent", label: "Sub-agent concurrency", kind: settingKindInt,
			min: floatPtr(1), max: floatPtr(64), apply: settingApply,
		},
		{
			key: "subagent.maxSteps", label: "Sub-agent step limit", kind: settingKindInt,
			min: floatPtr(0), max: floatPtr(10000), apply: settingApply,
		},
		{
			key: "images.block", label: "Block images", kind: settingKindBool,
			apply: settingApply,
		},
		{
			key: "tools.limits.bash.lines", label: "Bash line limit", kind: settingKindInt,
			min: floatPtr(1), apply: settingApply,
		},
		{
			key: "tools.limits.bash.bytes", label: "Bash byte limit", kind: settingKindInt,
			min: floatPtr(1), apply: settingApply,
		},
		{
			key: "tools.limits.read.lines", label: "Read line limit", kind: settingKindInt,
			min: floatPtr(1), apply: settingApply,
		},
		{
			key: "tools.limits.read.bytes", label: "Read byte limit", kind: settingKindInt,
			min: floatPtr(1), apply: settingApply,
		},
	}
}

// settingApply is the plain session-override apply most keys take: the value
// lands as an override with whatever live effect SetSessionSetting owns.
func settingApply(c Console, key string, v any) error {
	return c.SetSessionSetting(key, v)
}

// applyModelSetting switches the session onto one model, live effects and the
// model-change entry included. Persistence to a file layer is the caller's.
func applyModelSetting(c Console, name string) error {
	m, err := c.Models().Resolve(name)
	if err != nil {
		return err
	}
	c.SetModel(m)
	return nil
}

// applySubagentModel records the sub-agent model key, applied at the next
// spawn. The key must resolve so a typo cannot silently fall back to the
// session model.
func applySubagentModel(c Console, name string) error {
	if _, err := c.Models().Resolve(name); err != nil {
		return err
	}
	return c.SetSessionSetting("subagent.model", name)
}

// applyReasoningLevel sets the live reasoning level and records the override.
// Only a level the active model offers is accepted, matching the picker.
func applyReasoningLevel(c Console, name string) error {
	supported := llm.LevelsFor(c.Models().Active())
	lvl, ok := llm.ParseLevel(name)
	if !ok || !slices.Contains(supported, lvl) {
		return fmt.Errorf("reasoning level must be one of %s", levelsList(supported))
	}
	setReasoningField(c, func(rc *llm.ReasoningConfig) { rc.Level = lvl })
	return c.SetSessionSetting("reasoning.level", lvl.String())
}

// applyReasoningRetain sets the retention policy, keeping every other field.
func applyReasoningRetain(c Console, name string) error {
	p, ok := llm.ParseRetain(name)
	if !ok {
		return fmt.Errorf("retention policy must be one of %s", strings.Join(retainPolicies, "|"))
	}
	setReasoningField(c, func(rc *llm.ReasoningConfig) { rc.Retain = p })
	return c.SetSessionSetting("reasoning.retain", p.String())
}

// applyReasoningHide flips whether thinking streams, keeping every other field.
func applyReasoningHide(c Console, hide bool) error {
	setReasoningField(c, func(rc *llm.ReasoningConfig) { rc.Hide = hide })
	return c.SetSessionSetting("reasoning.hide", hide)
}

// SettingRows lists every wire-editable /settings row in menu order, each
// carrying its resolved value, source layer and editability.
func SettingRows(c Console) []SettingRow {
	specs := settingSpecs()
	out := make([]SettingRow, 0, len(specs))
	for _, spec := range specs {
		out = append(out, settingRowFor(c, spec))
	}
	return out
}

// SetSetting validates and applies one setting as a session override with its
// live effects, optionally copying the value to a config file layer, then
// reports the row after the change. Errors: ErrUnknownSetting, a value that
// fails validation, ErrSettingUnavailable wrapping why, and whatever the apply
// itself reports (an unresolvable model key among them).
func SetSetting(c Console, key string, raw json.RawMessage, persist string) (SettingRow, error) {
	spec, ok := settingSpecByKey(key)
	if !ok {
		return SettingRow{}, fmt.Errorf("%w: %q", ErrUnknownSetting, key)
	}
	switch persist {
	case "", "user", "project":
	default:
		return SettingRow{}, fmt.Errorf(`persist must be "user" or "project", not %q`, persist)
	}
	if len(raw) == 0 {
		return SettingRow{}, fmt.Errorf("%s needs a value", spec.label)
	}
	v, err := spec.parse(raw)
	if err != nil {
		return SettingRow{}, err
	}
	if reason := specOff(c, spec); reason != "" {
		return SettingRow{}, fmt.Errorf("%w: %s", ErrSettingUnavailable, reason)
	}
	if err := spec.apply(c, spec.key, v); err != nil {
		return SettingRow{}, err
	}
	if persist != "" {
		if err := c.SaveSetting(persist, key, v); err != nil {
			return SettingRow{}, err
		}
	}
	return settingRowFor(c, spec), nil
}

// settingSpecByKey finds one row's spec.
func settingSpecByKey(key string) (settingSpec, bool) {
	for _, spec := range settingSpecs() {
		if spec.key == key {
			return spec, true
		}
	}
	return settingSpec{}, false
}

// specOff reports why a row cannot be edited right now, "" when it can.
func specOff(c Console, spec settingSpec) string {
	if spec.off == nil {
		return ""
	}
	return spec.off(c)
}

// settingRowFor renders one row's current state.
func settingRowFor(c Console, spec settingSpec) SettingRow {
	row := SettingRow{Label: spec.label, Key: spec.key, Editable: true}
	if reason := specOff(c, spec); reason != "" {
		row.Editable = false
		row.Reason = reason
	}
	if spec.current != nil {
		row.Value, row.Source = spec.current(c)
		return row
	}
	if raw, src, ok := c.Settings().Explain(spec.key); ok {
		row.Value, row.Source = raw, orDefault(src)
		return row
	}
	// unset bools carry their typed zero, so a controller renders the
	// effective default rather than nothing; other shapes report absence
	if spec.kind == settingKindBool {
		row.Value, row.Source = json.RawMessage("false"), "default"
	}
	return row
}

// parse validates raw against the spec's shape and bounds, returning the
// typed value the apply takes.
func (s settingSpec) parse(raw json.RawMessage) (any, error) {
	bad := func(want string) error {
		return fmt.Errorf("%s must be %s", s.label, want)
	}
	boundsErr := func() error {
		switch {
		case s.min != nil && s.max != nil:
			return fmt.Errorf("%s must be between %g and %g", s.label, *s.min, *s.max)
		case s.min != nil:
			return fmt.Errorf("%s must be at least %g", s.label, *s.min)
		case s.max != nil:
			return fmt.Errorf("%s must be at most %g", s.label, *s.max)
		default:
			return fmt.Errorf("%s is out of range", s.label)
		}
	}
	switch s.kind {
	case settingKindBool:
		var v bool
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, bad("a boolean")
		}
		return v, nil
	case settingKindInt:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, bad("a number")
		}
		if f != math.Trunc(f) {
			return nil, bad("a whole number")
		}
		if (s.min != nil && f < *s.min) || (s.max != nil && f > *s.max) {
			return nil, boundsErr()
		}
		return int(f), nil
	case settingKindFloat:
		var f float64
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, bad("a number")
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, bad("a finite number")
		}
		if s.positive && f <= 0 {
			return nil, fmt.Errorf("%s must be a number greater than zero", s.label)
		}
		if (s.min != nil && f < *s.min) || (s.max != nil && f > *s.max) {
			return nil, boundsErr()
		}
		return f, nil
	case settingKindString, settingKindModel:
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, bad("a string")
		}
		if strings.TrimSpace(v) == "" {
			return nil, bad("a non-empty string")
		}
		if len(s.enums) > 0 && !slices.Contains(s.enums, v) {
			return nil, fmt.Errorf("%s must be one of %s", s.label, strings.Join(s.enums, "|"))
		}
		return v, nil
	case settingKindTools:
		var v []string
		if err := json.Unmarshal(raw, &v); err != nil {
			return nil, bad("a list of tool names")
		}
		return v, nil
	default:
		return nil, fmt.Errorf("%s has no value shape", s.label)
	}
}

// mustSettingJSON renders one row value, panicking only on unmarshalable
// shapes the callers control.
func mustSettingJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
