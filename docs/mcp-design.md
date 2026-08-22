# MCP client and server registration

How `pkg/mcp` turns servers declared in `mcp.json` into ordinary agent tools, why
the protocol layer is delegated to `github.com/mark3labs/mcp-go`, and the invariants
that keep MCP from leaking past its boundary. It is the substrate the extension protocol
rides on — an ajent extension *is* a registered MCP server.

## What it is

`pkg/mcp` connects to stdio and network (Streamable HTTP, legacy SSE) MCP servers,
bridges each exposed tool into `agent.Tool`, and supervises every configured server's
lifecycle. It delivers three things the rest of ajent consumes as plain facts:

- **Configuration** — merged from `~/.ajent/mcp.json` then `<workspace>/.ajent/mcp.json`.
- **Tools** — remote tools appear in the ordinary registry, namespaced to avoid
  collisions, subject to per-server filtering and enable state.
- **Lifecycle** — every server loads eagerly, in full, just before the user's
  first message; re-discovery on `tools/list_changed`, disconnect/reload through `/mcp`.

## Boundary rules

The dependency edge is load-bearing. The protocol layer (transports, JSON-RPC,
version negotiation, OAuth) is delegated to mcp-go; what ajent owns is the boundary.

- **`pkg/mcp ↛ pkg/tools`, `pkg/tui`, `pkg/command`, `pkg/refs`.** It imports only
  `agent`, `config`, `llm` and mcp-go. Everything it needs from the registry or front
  end arrives as an interface declared here (`Registrar`) or is adapted in `main.go`
  / `console.go`. This keeps the dependency isolated so the library can be replaced
  without touching the registry, TUI or session.
- **mcp-go wire types never escape `pkg/mcp`.** The bridge emits our own `agent.Tool`;
  discovery returns our own `ToolDef`/`Resource`/`PromptDef`. The extension host sees a
  raw seam (`Request`/`Handle`) over the transport, not mcp-go structs.
- **A tool of unknown effect does not run in an unobserved agent.** Read-only marking
  (from `annotations.readOnlyHint` OR config globs) defaults to *not* read-only; only a
  server or the user opts a tool into sub-agent publication. This is recorded as
  registry metadata, and the sub-agent tool set reads it: a marked MCP tool joins a
  sub-agent's `read`, `grep`, `find`, `ls` so delegated investigation can call remote
  read-only tools too.

## Configuration (`config.go`)

A server's config carries its stdio command plus args and env overrides, or a
network base URL with headers and an optional transport selection (`http`
default, legacy `sse`); enable state (nil means enabled), allow/deny tool globs,
exact exclusions from registration entirely, extra tools marked safe for
sub-agents, and a per-call timeout.

`LoadConfig(workspace)` reads user then project files and merges **by server name with
whole-entry replacement** — a project entry replaces the user's for that server in full,
never field-by-field. It deliberately does not reuse `config.Merge`, whose deep fold
would merge entries key by key against this spec.

- The read path follows `pkg/llm/config.go`'s `LoadFile`: `RelaxJSON` → unmarshal →
  unknown-key and duplicate-key warnings returned as strings for the caller to surface.
- `${env:VAR}` interpolation (the only form) applies to `Env`, `Headers` and `URL`. An
  unset variable is a **clear error naming it** — never an empty header or value.
- Validation rejects a server declaring both `command` and `url` (or neither), and
  `transport: sse` on a stdio server. There are no startup modes: every configured
  server connects eagerly, so the legacy `startup` key is unrecognized (a warning).

## Client wrapper (`client.go`)

A `Client` is obtained by connecting to a named server; it lists the remote
tools, calls one with raw JSON arguments and an output writer, pings, and closes.

- **stdio** — `NewStdioMCPClientWithOptions` with a command func that sets
  `Setpgid` and builds the child env from the parent plus config overrides. The
  resulting `*exec.Cmd` is kept on the `Client`; `Close` calls mcp-go's close then
  sweeps the whole process group (`syscall.Kill(-pid, SIGKILL)`) so grandchildren do
  not outlive a server torn down mid-session.
- **network** — Streamable HTTP (POST + SSE), or legacy SSE keyed off `transport: sse`.
- `Initialize` errors wrap the library's version-mismatch error naming both versions,
  rather than failing obscurely.

### Schema fidelity

`mcp.ToolInputSchema` is lossy — it keeps only a subset of keywords on round-trip. So
`Tools()` issues raw `tools/list` through the transport and decodes each entry's
`inputSchema` into a `json.RawMessage`, preserving the server's schema **byte for byte**
(proof that the raw request seam works). Pagination follows `nextCursor`. A tool whose
schema is not an object, or that has no name/schema, is skipped with one warning rather
than failing the whole list.

### Raw seams

`Request(ctx, method, params)` sends any JSON-RPC request and returns the raw result.
`Handle(method, h)` installs a handler for an incoming server→client method via the
transport's `BidirectionalInterface`, replacing mcp-go's handlers after `Start` and
re-implementing `ping` itself (we set no sampling/elicitation handlers, so nothing is
lost).

### Result mapping (`result.go`)

Text content becomes a text block; image and audio become short placeholders
(`[image omitted: <mime>]`, `[audio result …]`) since image processing is separate work;
embedded resources / resource links become text references (uri + description);
structured content with empty `content` falls back to its raw JSON. `isError` maps onto
`Result.IsError`.

## Bridging into the registry (`bridge.go`)

A remote tool definition and its owning client are turned into an ordinary
`agent.Tool` for one server.

The isolation seam: a remote tool becomes an ordinary `agent.Tool`, so `/tools`,
permissions, token accounting and the sub-agent treat it like any built-in.

- **Namespacing** — `Name()` returns `server__tool`; this is what the model sees and
  what appears in the transcript, so stability matters. `Label()` shows the bare tool
  name when unambiguous.
- **Mode** — serial unless read-only, which may run parallel with other reads.
- **Timeout** — per-call cap from config, default 60s, clamped to a max, mirroring
  `bash.go`.
- **Errors are results, never Go errors.** A transport failure returns an error result
  plus a notice so the turn continues; it does not abort. The model adapts.
- **Notifications never block mcp-go's reader.** The stdio transport delivers every
  notification synchronously on its single stdout-reader goroutine, so any handler that
  does blocking I/O inline (e.g. re-discovering after `list_changed`) would wait for a
  response the same blocked reader can never return — deadlocking the whole server.
  `Client.OnNotification` therefore dispatches each registered handler to its own
  goroutine at this boundary, so no current or future ajent handler can ever stall mcp-go.
- **Progress** — mcp-go's `notifications/progress` routes to `out.Write` (one line per
  notification) via a token→output map on the `Client`.
- Output volume is bounded with head/tail elision so one bridged call cannot flood the
  model.


## Server manager (`manager.go`)

One supervisor owns every configured server's lifecycle; `main.go` passes a registry
adapter and notice/status callbacks.

One supervisor owns every configured server's lifecycle; `main.go` passes a registry
adapter and notice/status callbacks. The adapter registers a source's tool under an
explicit state, unregisters a whole source, lists a source's enabled, disabled or all
names (so live enable state survives re-registration), and marks extra tools read-only.

The local `State` enum mirrors the registry's (Disabled / Enabled) so this
package stays free of `pkg/tools`.

- **First-message load (`LoadOnFirstMessage`)** — there is no startup spawn. Every
  server (including config-disabled ones) is connected in full, exactly once, just
  before the user's first message is assembled into a turn. A config-disabled server
  still connects so its tools stay visible and toggleable in `/tools`, but `register()`
  marks each of them `StateDisabled` — known and unchecked, never callable by default.
  Loading here rather than at session start means any `/tools` or `/mcp` change made up
  to that point takes effect — nothing is registered (and no process spawned) before the
  first message.
- **On-demand load for `/tools` and `/mcp`** — because loading happens on the first
  prompt, a pre-first-prompt `LoadOnFirstMessage` would otherwise leave MCP tools out of
  the free-select `/tools` picker and show stale tool counts in `/mcp list`. The pump
  therefore also triggers the (idempotent) load when either command is dispatched, so
  their tools are visible and toggleable before any message has been sent.
- **Resume ordering invariant.** The persisted enabled set is applied before MCP has
  registered anything, so those names would be dropped. `Options.Restore` (the session's
  `tools.enabled`) is therefore consulted at registration: a server exposes everything by
  default; the restored subset stays on and the rest are off.
- **Live state survives re-registration.** Before unregistering on reconnect or
  `list_changed`, the manager captures `EnabledNames(source)` and forces those back to
  enabled, so a tool-list refresh does not reset what is exposed.
- **`tools/list_changed`** triggers re-discovery: unregister source, register fresh,
  preserving live enable state. It runs through `rediscan`, which serializes per server —
  a second notification while one pass is in flight is coalesced (the running pass reads
  the current tool set anyway) — and bounds each pass with its own timeout so an
  unresponsive server surfaces an error rather than leaking a goroutine or hanging.
  Resources/prompts changes trigger best-effort capability refresh on reconnects; both
  paths are safe to do blocking I/O because notifications arrive asynchronously from the
  client (see *Notifications never block mcp-go's reader*).
- **Disconnect / Reload.** `/mcp disconnect` closes and unregisters without removing the
  config. `Reload` re-reads `mcp.json`, disconnects removed servers, connects newly added
  ones eagerly, applies config changes in place.

### Reconnection

A server that dies mid-session must produce a clear error result on any call into it —
never a hang — and be reconnected with capped backoff after repeated failures, at which
point its tools are unregistered so the model does not call into nothing. `failures`
tracks consecutive connect failures for the notice/status path (`reconnecting (n)`).


## Registry integration (`pkg/tools/registry.go`)

MCP mutates the registry from notification goroutines while the loop reads it, so every
method takes a lock. The single enabled bool became two states — known-but-disabled
(enabled in the prompt and callable). There is no deferred state: every configured server
is connected in full on first-message load, so each bridged tool registers as one of
these two.

- `Schemas()` includes only `StateEnabled`; `Names()` stays enabled-only (it feeds
  state + transcript); `Get()` answers Enabled tools only.
- `SetEnabled(names)` replaces the enabled set. `Enable(names)` widens from either state —
  what `/tools` calls after the first prompt; there is no other promotion method.
- `RegisterState(source, t, s)`, `Unregister(source)`, `BySource`, `EnabledNames(source)`,
  and read-only metadata (`MarkReadOnly`/`ReadOnly`) serve the manager; the sub-agent
  tool filter is their consumer (a marked remote tool joins a child's set).
  Every mutator nils the schema cache.

## Front end wiring

- `/mcp` lists servers (status, transport, tool count, latency), connects/disconnects,
  shows logs from a per-server bounded ring buffer fed by stdio stderr plus protocol
  lines, and reloads config.
- `pkg/command` declares its own small interfaces (`MCPServers`, `MCPGroup`,
  `MCPServerStatus`) so it never imports `pkg/mcp`; `main.go`'s adapters back them with
  the real manager.
- `/tools` groups MCP tools under a `mcp: <name>` header carrying tool count and
  connection state.

## Status

Config, client, bridge and manager lifecycle (first-message load of every server,
disconnect/reload), `/mcp` and read-only marking are implemented and tested against
mcp-go v0.58.0. Deferred loading (`_search`/`_load`), the lazy tool-list cache and all
startup modes have been removed: every configured server is connected in full on
`LoadOnFirstMessage`, so there is no placeholder registration, schema-drift path,
prompt-bloat trade-off or eager/manual distinction to maintain.
Stdio notification handling is deadlock-safe: `Client.OnNotification` dispatches handlers
off mcp-go's reader goroutine and `list_changed` re-discovery runs serialized with a
bounded context (see *Notifications never block mcp-go's reader*). The reconnection path
described above is partially wired — a call into a dead server already returns an error
result rather than hanging, but the backoff loop that unregisters tools on repeated
failure was still being completed at this writing; treat "Reconnection" as the invariant
to satisfy.
