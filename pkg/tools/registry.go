package tools

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
)

// Registry holds the tool set and its enable state. It satisfies agent.ToolSet
// so the loop reads tools straight off it. MCP notification goroutines mutate it
// while the loop reads, so every method takes the mutex.
type Registry struct {
	mu      sync.RWMutex
	tools   []registeredTool // declaration order drives Names/Schemas
	schema  []llm.ToolSchema // cached, invalidated by any state change
	guards  []Guard          // ordered; first non-allow wins inside Execute
	asker   Asker            // consulted on ActionAsk, nil denies
	tracker *Tracker         // the read tracker shared by read/write/edit, nil when none
}

// SourceBuiltin is the source label for tools registered by the core. MCP
// servers and extensions register under their own names.
const SourceBuiltin = "builtin"

// State is a tool's visibility to the model.
type State uint8

const (
	StateDisabled State = iota // known, not in the prompt, not callable
	StateEnabled               // in the prompt and callable
)

// registeredTool pairs a tool with its source label, enable state and whether
// it is safe to publish read-only (MCP annotations or config globs).
type registeredTool struct {
	tool     agent.Tool
	source   string // who registered it, for /tools grouping
	state    State
	readOnly bool // safe to expose to a sub-agent; default is not
}

func boolState(b bool) State {
	if b {
		return StateEnabled
	}
	return StateDisabled
}

// New returns an empty registry with no guards.
func New() *Registry { return &Registry{} }

// Register adds t to the registry under the builtin source, enabled when
// defaultEnabled is true. Order of registration drives Names and Schemas.
func (r *Registry) Register(t agent.Tool, defaultEnabled bool) {
	r.RegisterState(SourceBuiltin, t, boolState(defaultEnabled))
}

// RegisterFrom adds t to the registry under source, enabled when defaultEnabled
// is true. Source groups the tool in /tools (builtin, an MCP server name, an
// extension name); order of registration drives Names and Schemas.
func (r *Registry) RegisterFrom(source string, t agent.Tool, defaultEnabled bool) {
	r.RegisterState(source, t, boolState(defaultEnabled))
}

// RegisterState adds t under source with the given state. MCP servers register
// their bridged tools through this.
func (r *Registry) RegisterState(source string, t agent.Tool, s State) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = append(r.tools, registeredTool{tool: t, source: source, state: s})
	r.schema = nil // schema cache is stale until rebuilt
}

// Unregister drops every tool registered under source. Used on disconnect and
// re-discovery so a dead server's tools stop being offered.
func (r *Registry) Unregister(source string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools = bulk.SliceFilter(func(rt registeredTool) bool { return rt.source != source }, r.tools)
	r.schema = nil
}

// AddGuard appends g to the guard chain. Guards run in registration order and
// first non-allow wins.
func (r *Registry) AddGuard(g Guard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guards = append(r.guards, g)
}

// DryRunner is implemented by tools that can predict whether a call would fail,
// so the permission layer skips prompts for doomed calls and lets Execute return
// its natural error.
type DryRunner interface {
	DryRun(call agent.ToolCall) error
}

// Previewer is implemented by tools that can render what a call would change, so
// the approval dialog shows content or a diff instead of raw arguments.
type Previewer interface {
	Preview(call agent.ToolCall) (path, before, after string, err error)
}

// Preview reports whether call's effect is previewable and returns its path with
// the current and resulting text. Tools without one report ok=false.
func (r *Registry) Preview(call agent.ToolCall) (string, string, string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.tools {
		if rt.tool.Name() != call.Name {
			continue
		}
		pv, ok := rt.tool.(Previewer)
		if !ok {
			return "", "", "", false
		}
		path, before, after, err := pv.Preview(call)
		if err != nil {
			return "", "", "", false
		}
		return path, before, after, true
	}
	return "", "", "", false
}

// DryRun reports whether call would fail before running. Tools without a dry run
// (or an unknown name) report nil, so a caller only skips a prompt on a definite
// failure rather than guessing.
func (r *Registry) DryRun(call agent.ToolCall) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.tools {
		if rt.tool.Name() != call.Name {
			continue
		}
		d, ok := rt.tool.(DryRunner)
		if !ok {
			return nil // cannot predict; do not skip the prompt on uncertainty
		}
		return d.DryRun(call)
	}
	return nil // unknown tool: leave it to Execute's natural error path
}

// guardSnapshot returns an immutable copy of the guards and asker under read
// lock, so Execute runs a stable chain against concurrent registration.
func (r *Registry) guardSnapshot() ([]Guard, Asker) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Clone(r.guards), r.asker
}

// Enabled returns every currently enabled tool in declaration order.
func (r *Registry) Enabled() []agent.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []agent.Tool
	for _, rt := range r.tools {
		if rt.state == StateEnabled {
			out = append(out, rt.tool)
		}
	}
	return out
}

// SetEnabled replaces the enabled set with names. Unknown names are ignored;
// currently enabled tools not listed become disabled. Use Enable to widen the
// set within a session instead.
func (r *Registry) SetEnabled(names []string) {
	want := bulk.SliceToSet(names)
	r.mu.Lock()
	for i := range r.tools {
		if _, ok := want[r.tools[i].tool.Name()]; ok {
			r.tools[i].state = StateEnabled
		} else if r.tools[i].state == StateEnabled {
			r.tools[i].state = StateDisabled
		}
	}
	r.schema = nil // the tool block in the prompt changed, so bust the cache
	r.mu.Unlock()
}

// Enable additively enables the named tools from either state, leaving others
// untouched. Unknown names are ignored. The enabled set only widens within a
// session, so this is the /tools path after the first prompt; SetEnabled is the
// free-selection path before it.
func (r *Registry) Enable(names []string) {
	want := bulk.SliceToSet(names)
	r.mu.Lock()
	for i := range r.tools {
		if _, ok := want[r.tools[i].tool.Name()]; ok {
			r.tools[i].state = StateEnabled
		}
	}
	r.schema = nil
	r.mu.Unlock()
}

// Get returns a guard-wrapped callable tool by name. Only enabled tools answer;
// disabled ones do not. Use Lookup when a caller needs the tool regardless of state.
func (r *Registry) Get(name string) (agent.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.tools {
		if rt.state != StateEnabled || rt.tool.Name() != name {
			continue
		}
		return &guardedTool{t: rt.tool, reg: r}, true
	}
	return nil, false
}

// Lookup returns a guard-wrapped tool by name regardless of enable state. Use
// Get when the call should respect the enabled set (agent-initiated calls); a
// user-explicit @dir listing or ! shell command runs through Lookup so a
// disabled tool still serves the direct request.
func (r *Registry) Lookup(name string) (agent.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []agent.Tool
	for _, rt := range r.tools {
		if rt.state == StateDisabled {
			out = append(out, rt.tool)
		}
	}
	return out
}

// All returns every registered tool in declaration order regardless of enable
// state, for the pre-first-prompt /tools picker that selects freely.
func (r *Registry) All() []agent.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]agent.Tool, 0, len(r.tools))
	for _, rt := range r.tools {
		out = append(out, rt.tool)
	}
	return out
}

// BySource returns every tool registered under source in declaration order,
// regardless of state. The /tools grouping and the MCP manager use it to see a
// server's full offering.
func (r *Registry) BySource(source string) []agent.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []agent.Tool
	for _, rt := range r.tools {
		if rt.source == source {
			out = append(out, rt.tool)
		}
	}
	return out
}

// EnabledNames returns the currently enabled names registered under source. The
// MCP manager captures this before re-registering a server so a live tool-list
// refresh does not reset which tools are exposed.
func (r *Registry) EnabledNames(source string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, rt := range r.tools {
		if rt.source == source && rt.state == StateEnabled {
			out = append(out, rt.tool.Name())
		}
	}
	return out
}

// DisabledNames returns the currently disabled (inactive) names registered under
// source. The MCP status ratio treats a config-disabled or /tools-deselected tool
// as inactive but still discovered.
func (r *Registry) DisabledNames(source string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, rt := range r.tools {
		if rt.source == source && rt.state == StateDisabled {
			out = append(out, rt.tool.Name())
		}
	}
	return out
}

// AllNames returns every registered name under source, regardless of state. The
// MCP status ratio counts a server's live offering from it.
func (r *Registry) AllNames(source string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, rt := range r.tools {
		if rt.source == source {
			out = append(out, rt.tool.Name())
		}
	}
	return out
}

// MarkReadOnly records that the named tools are safe to publish read-only. Unknown
// names are ignored; the mark lives on the (source, tool) pair and is dropped with
// an Unregister. The sub-agent bridge filters its published set on this metadata.
func (r *Registry) MarkReadOnly(names []string) {
	want := bulk.SliceToSet(names)
	r.mu.Lock()
	for i := range r.tools {
		if _, ok := want[r.tools[i].tool.Name()]; ok {
			r.tools[i].readOnly = true
		}
	}
	r.mu.Unlock()
}

// ReadOnly reports whether name is marked safe to publish read-only.
func (r *Registry) ReadOnly(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.tools {
		if rt.tool.Name() == name {
			return rt.readOnly
		}
	}
	return false
}

// Source returns the registration source label for name, or empty when unknown.
func (r *Registry) Source(name string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
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
	r.mu.Lock() // caches into r.schema, so it needs the write lock
	defer r.mu.Unlock()
	if r.schema != nil {
		return slices.Clone(r.schema)
	}
	var out []llm.ToolSchema
	for _, rt := range r.tools {
		if rt.state != StateEnabled {
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
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []string
	for _, rt := range r.tools {
		if rt.state == StateEnabled {
			out = append(out, rt.tool.Name())
		}
	}
	return out
}

var _ agent.ToolSet = (*Registry)(nil)

// guardedTool runs the guard chain and any asker inside Execute.
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
	guards, asker := g.reg.guardSnapshot()
	for _, guard := range guards {
		d := guard(ctx, c)
		if d.Action == ActionAllow {
			continue
		}
		// Ask consults the asker when registered; an unresolved or re-ask result
		// refuses like a plain denial.
		if d.Action != ActionAsk || asker == nil {
			return denied(d.Reason)
		}
		resolved := asker(ctx, c, d)
		if resolved.Action != ActionAllow {
			reason := resolved.Reason
			if reason == "" { // ask treated as a denial carries the original reason
				reason = d.Reason
			}
			return denied(reason)
		}
	}
	return g.t.Execute(ctx, c, out)
}

// denied builds an error result carrying the denial's reason. Reasons are already
// self-framing ("refused …", "permission required …"), so no prefix is added.
func denied(reason string) (agent.ToolResult, error) {
	if strings.TrimSpace(reason) == "" { // guards always carry a reason; stay safe anyway
		reason = "permission required"
	}
	return agent.ToolResult{
		Content: llm.BlockList{llm.TextBlock{Text: reason}},
		IsError: true,
	}, nil
}
