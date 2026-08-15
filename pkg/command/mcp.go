package command

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// MCPGroup is one server's /tools group metadata, declared here so pkg/command
// does not import pkg/mcp. Source keys the grouping; Label is the rendered header.
type MCPGroup struct {
	Source string // "mcp: <name>", the /tools grouping key
	Label  string // full header text shown above the server's tools
}

// MCPServerStatus is one /mcp row, declared here so pkg/command does not import
// pkg/mcp.
type MCPServerStatus struct {
	Name      string
	Transport string // stdio, http or sse
	Connected bool
	State     string // connected, disconnected, unresponsive, reconnecting (n)
	ToolCount int
	Latency   time.Duration
}

// mcpCommand manages configured MCP servers: list them, connect or disconnect,
// show logs and reload config. SplitCommand only splits the first word, so the
// verb is parsed here.
func mcpCommand(_ context.Context, arg string, c Console) error {
	s := c.MCP()
	if s == nil {
		c.Notify("MCP not available", levelWarn)
		return nil
	}
	verb, rest, _ := strings.Cut(strings.TrimSpace(arg), " ")
	rest = strings.TrimSpace(rest)

	switch verb {
	case "", "list":
		mcpList(c, s)
	case "connect":
		if rest == "" {
			c.Notify("usage: /mcp connect <name>", levelWarn)
			return nil
		}
		if err := s.Connect(context.Background(), rest); err != nil {
			c.Notify(err.Error(), levelError)
		} else {
			c.Notify("mcp "+rest+": connected", levelInfo)
		}
	case "disconnect":
		if rest == "" {
			c.Notify("usage: /mcp disconnect <name>", levelWarn)
			return nil
		}
		s.Disconnect(rest)
		c.Notify("mcp "+rest+": disconnected", levelInfo)
	case "logs":
		if rest == "" {
			c.Notify("usage: /mcp logs <name>", levelWarn)
			return nil
		}
		mcpLogs(c, s, rest)
	case "reload":
		if err := s.Reload(context.Background()); err != nil {
			c.Notify(err.Error(), levelError)
		} else {
			c.Notify("mcp: reloaded", levelInfo)
		}
	default:
		c.Notify(fmt.Sprintf("unknown /mcp verb %q; try connect, disconnect, logs or reload", verb), levelWarn)
	}
	return nil
}

// mcpList prints a markdown table of server status.
func mcpList(c Console, s MCPServers) {
	var b strings.Builder
	b.WriteString("# MCP servers\n")
	rows := s.Status(context.Background())
	if len(rows) == 0 {
		b.WriteString("\n_no servers configured in ~/.ajent/mcp.json_\n")
		c.Print(b.String())
		return
	}
	b.WriteString("\n| server | state | transport | tools | latency |\n")
	b.WriteString("|--------|-------|-----------|------:|---------:|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %d | %s |\n",
			r.Name, r.State, orDash(r.Transport),
			r.ToolCount, latencyStr(r.Latency))
	}
	c.Print(b.String())
}

// mcpLogs prints a server's recent stderr and protocol lines.
func mcpLogs(c Console, s MCPServers, name string) {
	lines := s.Logs(name)
	if len(lines) == 0 {
		c.Notify("mcp "+name+": no logs", levelInfo)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# mcp %s logs\n\n```\n", name)
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	b.WriteString("```\n")
	c.Print(b.String())
}

// orDash returns s, or an em dash when empty so table cells stay aligned.
func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// latencyStr renders a duration compactly for table cells.
func latencyStr(d time.Duration) string {
	if d == 0 {
		return "—"
	}
	return d.Round(time.Millisecond).String()
}

// mcpCompletion offers verbs, then server names after connect/disconnect/logs.
func mcpCompletion(c Console) func(prefix string) []string {
	return func(prefix string) []string {
		s := c.MCP()
		if s == nil {
			return nil
		}
		verb, rest, hasSpace := strings.Cut(prefix, " ")
		switch {
		case !hasSpace:
			out := make([]string, 0, 5)
			for _, v := range []string{"connect", "disconnect", "logs", "reload"} {
				if verb == "" || strings.HasPrefix(v, verb) {
					out = append(out, v)
				}
			}
			return out
		case slices.Contains([]string{"connect", "disconnect", "logs"}, verb):
			names := s.ServerNames()
			var filtered []string
			for _, n := range names {
				if rest == "" || strings.HasPrefix(n, rest) {
					filtered = append(filtered, n)
				}
			}
			return filtered
		default:
			return nil
		}
	}
}
