package command

import (
	"context"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// notice levels re-exported for handlers
const (
	levelInfo  = tui.LevelInfo
	levelWarn  = tui.LevelWarn
	levelError = tui.LevelError
)

// RegisterBuiltins installs /help, /model, /reasoning, /usage, /compact, /tools,
// /session, /mcp, /reload, /agents, /settings and /exit into r against c. The driver adds
// its feature commands (/plan*, /init) to the same registry on top.
func RegisterBuiltins(r *Registry, c Console) {
	r.Register(Command{
		Name:        "help",
		Description: "list commands and keybindings, or ask the docs a question",
		Args:        "[question]",
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
		Name:        "copy",
		Description: "copy the last agent response to the clipboard",
		Handler:     copyCommand,
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
		Name:        "session",
		Description: "show or set this session's name",
		Args:        "[name]",
		Handler:     sessionCommand,
	})
	r.Register(Command{
		Name:        "mcp",
		Description: "manage MCP servers",
		Args:        "[connect|disconnect|logs|reload] [name]",
		Complete:    mcpCompletion(c),
		Handler:     mcpCommand,
	})
	r.Register(Command{
		Name:        "reload",
		Description: "re-read config.json, models.json, mcp.json and AGENTS.md",
		Handler:     reloadCommand,
	})
	r.Register(Command{
		Name:        "agents",
		Description: "list and stop sub-agents",
		Args:        "[list|stop <id>|all]",
		Complete:    agentsCompletion(c),
		Handler:     agentsCommand,
	})
	r.Register(Command{
		Name:        "settings",
		Description: "view and edit configuration",
		Args:        "[section]",
		Complete:    settingsCompletion(c),
		Handler:     settingsCommand,
	})
	r.Register(Command{
		Name:        "update",
		Description: "reinstall ajent from the latest release",
		Handler:     updateCommand,
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

// helpKeys is the curated keybinding summary bare /help appends. Curated, not
// exhaustive: docs/features/tui.md carries every combination across surfaces.
func helpKeys(b *strings.Builder) {
	lines := []string{
		"`↑`/`↓` — move a visual row; recall older/newer history once at the first/last character",
		"`Ctrl+P` / `Ctrl+N` — up/down, same as `↑`/`↓`",
		"`←`/`→` or `Ctrl+B`/`Ctrl+F` — by cell; `Alt+B`/`Alt+F`, `Ctrl+←`/`→`, `Alt+←`/`→` — by word",
		"`Home` / `Ctrl+A`, `End` / `Ctrl+E` — start/end of the visual row",
		"`Enter` submits; `Ctrl+J` or `Alt+Enter` insert a newline (rebindable, keymap config)",
		"`Backspace` deletes back, `Delete` forward; `Esc` with text clears the whole draft",
		"`Ctrl+K` — delete from cursor to end of line",
		"`Ctrl+U` — delete from start of line to cursor",
		"`Ctrl+W` or `Alt+Backspace` — delete the word before the cursor",
		"`Ctrl+Y` — restore what the last clear or kill removed (one level deep)",
		"`Ctrl+C` — with text clears the draft; interrupts a turn, or quits when idle",
		"`Ctrl+D` — quit on an empty editor; deletes forward otherwise",
		"`Ctrl+L` — redraw the screen",
		"`Ctrl+Z` — suspend to the shell",
		"`Ctrl+R` — search history as you type",
		"`Alt+↑` — pull the newest queued prompt back into the editor",
		"`Esc Esc` — rewind onto an earlier message while idle; `ctrl+x` in the picker copies the highlighted message",
		"`Tab` / `Shift+Tab` — completion forward and back; `/` at line start opens the command menu, `@` completes paths, `!`/`!!` lines complete like bash",
		"`!cmd` — run a shell command, staged ahead of your next message",
		"`!!cmd` — same run shown only to you: output never reaches model context or transcript",
		"`Ctrl+V` — probe the clipboard and attach a found image",
		"large bracketed pastes collapse to a `[pasted N lines]` placeholder, expanded on send",
		"`Shift+Tab` / `Shift+→` / `Shift+←` — cycle the permission mode, forward or back (out-of-band control event)",
	}
	for _, line := range lines {
		b.WriteString("- " + line + "\n")
	}
}

// helpCommand renders the command list and keybindings as markdown through
// Console.Print, closing with where questions go. With a question it delegates
// to Help, printing nothing itself.
func helpCommand(ctx context.Context, arg string, c Console) error {
	if q := strings.TrimSpace(arg); q != "" {
		return c.Help(ctx, q)
	}
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
	helpKeys(&b)
	b.WriteString("\nAsk about ajent itself with `/help <question>`.\n")
	c.Print(b.String())
	return nil
}

// filterPrefix returns the entries of names matching prefix case-insensitively,
// keeping their original case, or nil when nothing matches.
func filterPrefix(names []string, prefix string) []string {
	if prefix == "" {
		return names
	}
	lower := strings.ToLower(prefix)
	filtered := bulk.SliceFilterInPlace(func(n string) bool {
		return strings.HasPrefix(strings.ToLower(n), lower)
	}, names)
	if len(filtered) == 0 { // preserve nil (not empty) for no match
		return nil
	}
	return filtered
}
