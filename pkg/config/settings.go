package config

import "encoding/json"

// Settings is the typed surface of every configuration layer. Enum valued keys
// are stored as their text names and parsed by the caller (llm.ParseLevel,
// llm.ParseRetain, tui.ParseMode); providers and models stay raw so pkg/llm can
// fold them over its own schema without a config→llm import.
type Settings struct {
	Model       string          `json:"model,omitempty"`
	Reasoning   Reasoning       `json:"reasoning,omitzero"`
	Providers   json.RawMessage `json:"providers,omitempty"` // llm.ProviderConfig map
	Models      json.RawMessage `json:"models,omitempty"`    // llm.ModelConfig by "provider/id"
	Tools       Tools           `json:"tools,omitzero"`
	Permissions Permissions     `json:"permissions,omitzero"` // enforced by the tool guard chain
	Compaction  Compaction      `json:"compaction,omitzero"`
	UI          UI              `json:"ui,omitzero"`
	Extensions  Extensions      `json:"extensions,omitzero"` // loaded by the extension host
}

// Reasoning is the session reasoning choice. Level and Retain are text names.
type Reasoning struct {
	Level  string `json:"level,omitempty"`  // llm.Level name
	Retain string `json:"retain,omitempty"` // llm.RetainPolicy name
	Budget int    `json:"budget,omitempty"`
	Show   bool   `json:"show,omitempty"`
}

// Tools configures the enabled set and output bounds.
type Tools struct {
	Enabled []string   `json:"enabled,omitempty"`
	Limits  ToolLimits `json:"limits,omitzero"`
}

// ToolLimits is the configurable subset of pkg/tools' built-in output bounds,
// one Limit per tool. A zero field leaves that dimension at its default.
type ToolLimits struct {
	Bash      Limit `json:"bash,omitzero"`
	Read      Limit `json:"read,omitzero"`
	Find      Limit `json:"find,omitzero"`
	Grep      Limit `json:"grep,omitzero"`
	Ls        Limit `json:"ls,omitzero"`
	RefInject Limit `json:"refInject,omitzero"`
	RefTotal  Limit `json:"refTotal,omitzero"`
}

// Limit bounds one tool's output; a zero field means that dimension is unbounded.
type Limit struct {
	Lines int `json:"lines,omitempty"`
	Bytes int `json:"bytes,omitempty"`
}

// Compaction tunes automatic context reduction. Threshold is the fraction of
// the window (or an absolute token count) at which it fires.
type Compaction struct {
	Auto      bool    `json:"auto,omitzero"`
	Threshold float64 `json:"threshold,omitzero"`
}

// UI configures the terminal surface.
type UI struct {
	Render       string `json:"render,omitempty"` // tui.Mode name
	ShowCost     bool   `json:"showCost,omitzero"`
	ShowThinking bool   `json:"showThinking,omitzero"`
}

// Permissions configures the tool guard chain.
type Permissions struct {
	Mode string `json:"mode,omitempty"`
}

// Extensions configures extension loading.
type Extensions struct {
	Dir      string   `json:"dir,omitempty"`
	Disabled []string `json:"disabled,omitzero"`
}

// defaultsJSON is layer one, the compiled-in configuration. Keeping it a JSON
// literal rather than struct zero values lets Explain report "(default)" as an
// ordinary source and mirrors today's constants exactly.
const defaultsJSON = `{
  "reasoning": { "level": "medium", "retain": "wholeTurn", "show": true },
  "tools": {
    "enabled": ["read", "write", "edit", "bash"],
    "limits": {
      "bash":     { "lines": 4000, "bytes": 32768 },
      "read":     { "lines": 2000, "bytes": 536870912 },
      "find":     { "lines": 500 },
      "grep":     { "lines": 1000, "bytes": 131072 },
      "ls":       { "lines": 500 },
      "refInject":{ "lines": 500, "bytes": 32768 },
      "refTotal": { "bytes": 131072 }
    }
  },
  "compaction": { "auto": true, "threshold": 0.8 },
  "ui": { "render": "auto" }
}`

// Defaults returns the compiled-in configuration layer.
func Defaults() Layer {
	return Layer{Name: "default", Data: []byte(defaultsJSON)}
}
