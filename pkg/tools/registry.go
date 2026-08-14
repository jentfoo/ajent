package tools

import (
	"context"
	"slices"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// Registry holds the tool set and its enabled state. It satisfies agent.ToolSet
// so the loop reads tools straight off it.
type Registry struct {
	tools   []registeredTool // declaration order drives Names/Schemas
	schema  []llm.ToolSchema // cached, invalidated by SetEnabled
	guards  []Guard          // ordered; first non-allow wins inside Execute
	tracker *Tracker         // the read tracker shared by read/write/edit, nil when none
}

// SourceBuiltin is the source label for tools registered by the core. MCP
// servers and extensions register under their own names.
const SourceBuiltin = "builtin"

// registeredTool pairs a tool with its default-enabled flag and source label.
type registeredTool struct {
	tool           agent.Tool
	source         string // who registered it, for /tools grouping
	defaultEnabled bool
	enabled        bool
}

// New returns an empty registry with no guards.
func New() *Registry { return &Registry{} }

// Register adds t to the registry under the builtin source, enabled when
// defaultEnabled is true. Order of registration drives Names and Schemas.
func (r *Registry) Register(t agent.Tool, defaultEnabled bool) {
	r.RegisterFrom(SourceBuiltin, t, defaultEnabled)
}

// RegisterFrom adds t to the registry under source, enabled when defaultEnabled
// is true. Source groups the tool in /tools (builtin, an MCP server name, an
// extension name); order of registration drives Names and Schemas.
func (r *Registry) RegisterFrom(source string, t agent.Tool, defaultEnabled bool) {
	r.tools = append(r.tools, registeredTool{
		tool:           t,
		source:         source,
		defaultEnabled: defaultEnabled,
		enabled:        defaultEnabled,
	})
	r.schema = nil // schema cache is stale until rebuilt
}

// AddGuard appends g to the guard chain. Guards run in registration order and
// first non-allow wins.
func (r *Registry) AddGuard(g Guard) { r.guards = append(r.guards, g) }

// Enabled returns every currently enabled tool in declaration order.
func (r *Registry) Enabled() []agent.Tool {
	var out []agent.Tool
	for _, rt := range r.tools {
		if rt.enabled {
			out = append(out, rt.tool)
		}
	}
	return out
}

// SetEnabled replaces the enabled set with names. Unknown names are ignored;
// known ones not listed become disabled. Use Enable to widen the set within a
// session instead.
func (r *Registry) SetEnabled(names []string) {
	want := bulk.SliceToSet(names)
	for i := range r.tools {
		r.tools[i].enabled = false
	}
	for i := range r.tools {
		if _, ok := want[r.tools[i].tool.Name()]; ok {
			r.tools[i].enabled = true
		}
	}
	r.schema = nil // the tool block in the prompt changed, so bust the cache
}

// Enable additively enables the named tools, leaving others untouched. Unknown
// names are ignored. The enabled set only widens within a session, so this is
// the /tools path after the first prompt; SetEnabled is the free-selection path
// before it.
func (r *Registry) Enable(names []string) {
	want := bulk.SliceToSet(names)
	for i := range r.tools {
		if _, ok := want[r.tools[i].tool.Name()]; ok {
			r.tools[i].enabled = true
		}
	}
	r.schema = nil
}

// Get returns an enabled, guard-wrapped tool by name. Use Lookup when a caller
// needs the tool regardless of enabled state (e.g. @ running ls, ! checking
// bash): Get would silently refuse a disabled tool.
func (r *Registry) Get(name string) (agent.Tool, bool) {
	for _, rt := range r.tools {
		if !rt.enabled || rt.tool.Name() != name {
			continue
		}
		return &guardedTool{t: rt.tool, reg: r}, true
	}
	return nil, false
}

// Lookup returns a guard-wrapped tool by name regardless of enabled state. Use
// Get when the call should respect the enabled set (agent-initiated calls); a
// user-explicit @dir listing or ! shell command runs through Lookup so a
// disabled tool still serves the direct request.
func (r *Registry) Lookup(name string) (agent.Tool, bool) {
	for _, rt := range r.tools {
		if rt.tool.Name() != name {
			continue
		}
		return &guardedTool{t: rt.tool, reg: r}, true
	}
	return nil, false
}

// Disabled returns currently disabled tools in declaration order, for the
// post-first-prompt /tools mode that can only widen the set.
func (r *Registry) Disabled() []agent.Tool {
	var out []agent.Tool
	for _, rt := range r.tools {
		if !rt.enabled {
			out = append(out, rt.tool)
		}
	}
	return out
}

// All returns every registered tool in declaration order regardless of enabled
// state, for the pre-first-prompt /tools picker that selects freely.
func (r *Registry) All() []agent.Tool {
	out := make([]agent.Tool, 0, len(r.tools))
	for _, rt := range r.tools {
		out = append(out, rt.tool)
	}
	return out
}

// Source returns the registration source label for name, or empty when unknown.
func (r *Registry) Source(name string) string {
	for _, rt := range r.tools {
		if rt.tool.Name() == name {
			return rt.source
		}
	}
	return ""
}

// Tracker returns the read tracker shared by read/write/edit, or nil when none.
func (r *Registry) Tracker() *Tracker { return r.tracker }

// Schemas returns the enabled tool schemas in declaration order, cached.
func (r *Registry) Schemas() []llm.ToolSchema {
	if r.schema != nil {
		return slices.Clone(r.schema)
	}
	var out []llm.ToolSchema
	for _, rt := range r.tools {
		if !rt.enabled {
			continue
		}
		out = append(out, llm.ToolSchema{
			Name:        rt.tool.Name(),
			Description: rt.tool.Description(),
			Parameters:  rt.tool.Schema().Parameters,
		})
	}
	r.schema = out
	return slices.Clone(out)
}

// Names returns the enabled tool names in declaration order.
func (r *Registry) Names() []string {
	var out []string
	for _, rt := range r.tools {
		if rt.enabled {
			out = append(out, rt.tool.Name())
		}
	}
	return out
}

var _ agent.ToolSet = (*Registry)(nil)

// guardedTool runs the guard chain inside Execute. With no asker registered an
// ActionAsk is treated as a denial naming its reason.
type guardedTool struct {
	t   agent.Tool
	reg *Registry
}

func (g *guardedTool) Name() string { return g.t.Name() }
func (g *guardedTool) Label(c agent.ToolCall) string {
	return g.t.Label(c)
}
func (g *guardedTool) Description() string       { return g.t.Description() }
func (g *guardedTool) Schema() llm.ToolSchema    { return g.t.Schema() }
func (g *guardedTool) Mode() agent.ExecutionMode { return g.t.Mode() }

// Execute vets the call through every guard, then delegates to the wrapped tool.
// A denial becomes an error result carrying its reason and nothing touches disk.
func (g *guardedTool) Execute(ctx context.Context, c agent.ToolCall, out agent.Output) (agent.ToolResult, error) {
	for _, guard := range g.reg.guards {
		d := guard(ctx, c)
		switch d.Action {
		case ActionAllow:
			continue
		default: // Deny or Ask without an asker both refuse the call
			return agent.ToolResult{
				Content: llm.BlockList{llm.TextBlock{Text: "denied: " + d.Reason}},
				IsError: true,
			}, nil
		}
	}
	return g.t.Execute(ctx, c, out)
}
