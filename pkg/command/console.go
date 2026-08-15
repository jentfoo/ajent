package command

import (
	"context"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// MCPServers is the MCP manager's view /mcp needs, declared here so pkg/command
// does not import pkg/mcp. main.go's uiConsole returns the real manager.
type MCPServers interface {
	ServerNames() []string
	// LoadOnFirstMessage connects and registers every server in full, exactly once;
	// the driver calls it just before assembling the user's first message.
	LoadOnFirstMessage(ctx context.Context)
	Status(ctx context.Context) []MCPServerStatus
	Connect(ctx context.Context, name string) error
	Disconnect(name string)
	Reload(ctx context.Context) error // re-reads mcp.json and reconciles
	Logs(name string) []string
	Groups() []MCPGroup // per-server /tools group metadata
	RefreshStatus()     // republish the status ratio after tool-set changes
}

// Console is a command's view of the world: UI interactions, session, agent
// state, registries and config. It is an interface rather than a grab-bag struct
// so an extension host can back it over a wire protocol.
type Console interface {
	// Notify commits a marked line to history at the given level.
	Notify(msg string, level tui.Level)
	// Print renders markdown into history, the form /help uses.
	Print(markdown string)
	// Pick presents a filterable list and returns the chosen index, or
	// ErrCancelled on Esc.
	Pick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.PickOptions) (int, error)
	// MultiPick presents a filterable multi-select list and returns the chosen
	// indexes, or ErrCancelled on Esc.
	MultiPick(ctx context.Context, prompt string, items []tui.PickItem, opts tui.MultiPickOptions) ([]int, error)
	// Select presents options and returns the chosen index.
	Select(ctx context.Context, prompt string, options []tui.Option) (int, error)
	// Confirm asks a yes or no question.
	Confirm(ctx context.Context, prompt string) (bool, error)
	// Input prompts for one line of text.
	Input(ctx context.Context, label, placeholder string) (string, error)

	// Models returns the live model registry, the single source of truth for the
	// active model.
	Models() *llm.Registry
	// State returns the live agent state; handlers read and mutate it directly.
	State() *agent.State
	// Tools returns the live tool registry, the single source of truth for the
	// enabled set.
	Tools() *tools.Registry
	// MCP returns the MCP server manager, nil when no servers are configured or
	// wiring is unavailable. /mcp uses it to list and control servers.
	MCP() MCPServers
	// Commands returns the command registry, so /help can enumerate itself.
	Commands() *Registry
	// Settings returns the resolved configuration handle for this workspace.
	Settings() *config.Set
	// SaveSetting writes key into a config layer file ("user" or "project").
	SaveSetting(layer, key string, value any) error

	// SetModel makes m active, reflects it in the status line and agent state,
	// and records a model-change entry in the session.
	SetModel(m llm.Model)
	// SetReasoning sets the session reasoning level and persists it.
	SetReasoning(c llm.ReasoningConfig)
	// ToolsChanged persists the enabled set to the session so a resume keeps it.
	ToolsChanged()

	// Started reports whether a user prompt has been sent this session. The
	// /tools picker is unrestricted before it and widen-only after.
	Started() bool
	// Compact reduces the session context toward the compaction threshold.
	// instructions is an optional /compact <instructions> focus for the summariser.
	Compact(ctx context.Context, instructions string) error
	// Exit signals the driver to quit.
	Exit()
}
