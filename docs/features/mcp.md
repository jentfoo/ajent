# MCP

MCP servers add tools over the Model Context Protocol. ajent connects to
stdio and network servers declared in `mcp.json`, and every exposed tool
appears in the ordinary registry: toggleable from `/tools`, permission-gated
like any built-in, and visible to sub-agents when marked read-only. Permission
modes live in `permissions.md`, sub-agent behavior in `agents.md`, every other
slash command in `commands.md`.

## mcp.json

Servers live in two files:

- `~/.ajent/mcp.json` - personal servers, available in every workspace
- `<workspace>/.ajent/mcp.json` - project-specific servers

Both files merge by server name and a project entry replaces the user's for
the same server in full, never field by field. The loader accepts `//`
comments and trailing commas, warns on unknown or duplicate keys (last one
wins), and treats a missing, empty or comment-only file as no servers.

```jsonc
{
  "servers": {
    // stdio: ajent runs the command as a child process
    "context7": {
      "command": "npx", "args": ["-y", "@upstash/context7-mcp"],
      "env": { "API_KEY": "${env:CONTEXT7_API_KEY}" }
    },

    // network: streamable http by default, legacy sse with "transport"
    "deepwiki": {
      "url": "https://mcp.deepwiki.com/mcp",
      "headers": { "Authorization": "Bearer ${env:DEEPWIKI_TOKEN}" },
      "tools": { "allow": ["read_*"], "deny": ["write_*"] },
      "readOnly": true
    },

    // configured but never connected
    "playwright": {
      "command": "npx", "args": ["-y", "@playwright/mcp"],
      "enabled": false
    }
  }
}
```

Each server declares exactly one of `command` (stdio) or `url` (network), and
a declared `transport` must agree with it (`stdio` needs the command, `http`
or `sse` the url).

| Key | Meaning |
|---|---|
| `command`, `args`, `env` | the stdio child process, its arguments, and env overrides layered over ajent's own environment |
| `url`, `headers` | the network endpoint and request headers. `transport` selects `http` (default) or legacy `sse` |
| `enabled` | `false` configures the server without connecting. `true` or absent connects on startup |
| `tools.allow`, `tools.deny` | glob filters applied before registration. Deny wins over a matching allow, and an empty allow list admits everything not denied or excluded |
| `excludeTools` | exact tool names omitted from registration entirely |
| `readOnly` | tools to mark read-only: `true` for every tool of the server, or one glob / a list of globs (see below) |
| `timeout` | per-call cap as an ms number (`15000`) or a duration like `"90s"`. Defaults to 60 seconds and clamps at 10 minutes |

Globs match each tool's bare name, the one the server publishes, without the
`server__` prefix. So `"allow": ["read_*"]` matches ajent's registered
`deepwiki__read_wiki` through its original `read_wiki`.

`${env:VAR}` references in env values, headers and URLs expand at load. An
unset or empty variable is a clear error naming it, never an empty header.

A server key must match `[a-zA-Z0-9_-]{1,64}` without `__`, which is reserved
as the namespace separator between server name and tool name. One bad key or
one broken entry is skipped with a warning rather than failing the whole file.

## Marking tools read-only

The read-only mark says "this tool never changes anything" and buys three
things: it auto-runs without approval in the read-oriented permission modes
(`permissions.md`), joins sub-agent tool sets (`agents.md`), and may run in
parallel with other read-only calls instead of serially. An unmarked MCP tool
defaults to not read-only, no matter how harmless its description sounds.

The mark comes from two sources:

- the server's own declaration - a tool marked with the MCP read-only
  annotation (`annotations.readOnlyHint`) is honored as-is
- your config - the per-server `readOnly` key

```jsonc
{
  "servers": {
    // every tool of this server
    "deepwiki": { "url": "...", "readOnly": true },

    // only tools matching the globs; a bare string is one glob
    "github": {
      "command": "gh", "args": ["mcp", "serve"],
      "readOnly": ["get_*", "list_*", "search_*"]
    }
  }
}
```

Config marks are additive: your globs can mark a tool the server does not hint
at, but they never clear a declared read-only. A tool with neither source is
not read-only and always asks first.

For one-off auto-allows by exact name instead of effect class,
`permissions.safeCommands` accepts an entry that names a single `server__tool`
or a bare server name covering every tool it exposes (`permissions.md`).

## Loading and lifecycle

Every configured server connects in the background while you type, and tool
schemas are complete before your first prompt. A server's tools load once:
after the first message they cannot be unloaded, though adding tools later is
allowed at the cost of a prompt-cache miss.

Connections survive deaths:

- a stdio child that exits reconnects with capped backoff, its tools
  unregistered while it is down and restored when it comes back
- a network server has no death signal: one that stops answering surfaces as
  an error on the next call instead of a hang
- a server that announces its tool list changed is re-discovered in place,
  keeping deliberate `/tools` toggles

Only tools are bridged. Server resources and prompts are not exposed.

## /mcp

| Form | Behavior |
|---|---|
| `/mcp` | print a table of servers: name, state, transport, tool count, latency |
| `/mcp connect <name>` | connect a configured server now |
| `/mcp disconnect <name>` | close and unregister it without removing its config |
| `/mcp logs <name>` | recent stderr and protocol lines in a code block |
| `/mcp reload` | re-read both mcp.json files against the running session |

Server states read `connected`, `disconnected`, `unresponsive` and
`reconnecting (n)`. A dial failure is expected (offline or not yet started),
so it stays in `/mcp logs` rather than surfacing as a notice. Completion
suggests the verbs, then server names for connect, disconnect and logs.

### /mcp reload

Reload re-reads both files while the session keeps running: removed servers
are disconnected, new ones connected. Filter fields (`tools.allow/deny`,
`excludeTools`, `readOnly`, `enabled`) apply immediately by re-registering the
server's tools with its live toggles preserved, while connection fields
(`command`, `args`, `env`, `url`, `headers`, `transport`) take effect on the
server's next connect. A server a reload adds starts with its tools off until
`/tools` enables them.

## /tools interactions

Each configured server's tools appear in `/tools` grouped under one header row
carrying tool count and connection state, like
`mcp: deepwiki  (5 tools, connected)`. Built-ins sort first, then servers.
Config-disabled servers still appear so you can enable them for the session,
and they are the exception to commands.md's widen-only rule: a server added by
`/mcp reload`, or one enabled with `"enabled": false` in config, offers its
tools for turning on even after this run's first prompt, because the model has
not seen that tool block yet. The general picker rules (free selection before
the first prompt, widen-only after, session-saved selections) are described in
`commands.md`.

## Calling MCP tools

A bridged tool is named `server__tool` in the registry, the transcript and the
model's view, with the bare tool name shown as its label. Read-only tools run
parallel with other reads, everything else serially.

A call that exceeds its timeout or fails on transport becomes an error result
the model adapts to, never a hung turn, and the failure is recorded to that
server's `/mcp logs`. Results map onto the ordinary content model: text
becomes a text block, images become image blocks, anything else (audio
included) a short placeholder naming the media kind. Structured content with
no text falls back to its raw JSON. Incremental progress streams to your
output the same way a built-in tool's does.

## See also

- `permissions.md` - permission modes, approval dialogs, safe/denied matching
- `agents.md` - sub-agents and /plan, which inherit marked read-only MCP tools
- `commands.md` - every slash command and the input-line grammar
- `config.md` - config.json layers, `AJENT_*` bindings
