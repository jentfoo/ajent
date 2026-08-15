package command

import (
	"context"
	"strings"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// RegisterBuiltins installs /help, /usage, /model, /reasoning, /tools and /exit
// into r against c. The driver registers /settings, /compact, /resume, /cost
// and /init into the same registry.
func RegisterBuiltins(r *Registry, c Console) {
	r.Register(Command{
		Name:        "help",
		Description: "list commands and keybindings",
		Handler:     helpCommand,
	})
	r.Register(Command{
		Name:        "model",
		Description: "show or pick the active model",
		Args:        "[name]",
		Complete:    modelCompletion(c),
		Handler:     modelCommand,
	})
	r.Register(Command{
		Name:        "reasoning",
		Description: "show or set the reasoning level",
		Args:        "[level]",
		Complete: func(prefix string) []string {
			levels := llm.LevelsFor(c.Models().Active())
			out := make([]string, 0, len(levels))
			for _, lvl := range levels {
				out = append(out, lvl.String())
			}
			return filterPrefix(out, prefix)
		},
		Handler: reasoningCommand,
	})
	r.Register(Command{
		Name:        "usage",
		Description: "show session token usage and context status",
		Handler:     usageCommand,
	})
	r.Register(Command{
		Name:        "compact",
		Description: "reduce context toward the compaction threshold",
		Args:        "[instructions]",
		Handler:     compactCommand,
	})
	r.Register(Command{
		Name:        "tools",
		Description: "enable tools for this session",
		Handler:     toolsCommand,
	})
	r.Register(Command{
		Name:        "mcp",
		Description: "manage MCP servers",
		Args:        "[connect|disconnect|logs|reload] [name]",
		Complete:    mcpCompletion(c),
		Handler:     mcpCommand,
	})
	r.Register(Command{
		Name:        "settings",
		Description: "view and edit configuration",
		Args:        "[section]",
		Complete:    settingsCompletion(c),
		Handler:     settingsCommand,
	})
	r.Register(Command{
		Name:        "exit",
		Description: "quit",
		Handler: func(_ context.Context, _ string, c Console) error {
			c.Exit()
			return nil
		},
	})
}

// helpCommand renders a markdown list of commands through Console.Print.
func helpCommand(_ context.Context, _ string, c Console) error {
	var b strings.Builder
	b.WriteString("# Commands\n\n")
	for _, cmd := range c.Commands().List() {
		line := "- `/" + cmd.Name
		if cmd.Args != "" {
			line += " " + cmd.Args
		}
		line += "` — " + cmd.Description
		b.WriteString(line + "\n")
	}
	b.WriteString("\n# Keybindings\n\n")
	b.WriteString("- `↑`/`↓` — recall history / move line\n")
	b.WriteString("- `Ctrl+C` — interrupt a turn, or quit when idle\n")
	b.WriteString("- `Ctrl+D` — quit on an empty editor\n")
	b.WriteString("- `Esc` `Esc` — rewind onto an earlier message while idle\n")
	b.WriteString("- `/` at line start — command completion\n")
	b.WriteString("- `@` anywhere — path completion\n")
	b.WriteString("- `!cmd` — run a shell command, staged ahead of your next message\n")
	b.WriteString("- `Shift+Tab` — cycle the permission mode (out-of-band control event)\n")
	c.Print(b.String())
	return nil
}

// filterPrefix returns the entries of names that start with prefix, lowercased.
func filterPrefix(names []string, prefix string) []string {
	if prefix == "" {
		return names
	}
	var out []string
	for _, n := range names {
		if strings.HasPrefix(strings.ToLower(n), strings.ToLower(prefix)) {
			out = append(out, n)
		}
	}
	return out
}

// notice levels re-exported for handlers
const (
	levelInfo  = tui.LevelInfo
	levelWarn  = tui.LevelWarn
	levelError = tui.LevelError
)
