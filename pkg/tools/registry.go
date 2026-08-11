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
	tools  []registeredTool // declaration order drives Names/Schemas
	schema []llm.ToolSchema // cached, invalidated by SetEnabled
	guards []Guard          // ordered; first non-allow wins inside Execute
}

// registeredTool pairs a tool with its default-enabled flag.
type registeredTool struct {
	tool           agent.Tool
	defaultEnabled bool
	enabled        bool
}

// New returns an empty registry with no guards.
func New() *Registry { return &Registry{} }

// Register adds t to the registry, enabled when defaultEnabled is true. Order of
// registration drives Names and Schemas.
func (r *Registry) Register(t agent.Tool, defaultEnabled bool) {
	r.tools = append(r.tools, registeredTool{tool: t, defaultEnabled: defaultEnabled, enabled: defaultEnabled})
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

// SetEnabled flips the enabled set to names. Unknown names are ignored; known
// ones not listed become disabled.
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

// Get returns an enabled, guard-wrapped tool by name.
func (r *Registry) Get(name string) (agent.Tool, bool) {
	for _, rt := range r.tools {
		if !rt.enabled || rt.tool.Name() != name {
			continue
		}
		return &guardedTool{t: rt.tool, reg: r}, true
	}
	return nil, false
}

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
