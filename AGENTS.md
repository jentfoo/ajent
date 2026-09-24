## Project Overview

`ajent` is a lightweight CLI coding agent written in Go: a terminal front end over a
turn loop that streams from an LLM provider, runs tools, and persists every turn as a
resumable JSONL transcript. Single module, no provider SDKs, no TUI framework.

## Commands

```sh
make build        # -> ./bin/ajent
make test         # go test -short ./...
make test-all     # go test -race -cover ./...
make lint         # gofmt changed files, then golangci-lint + go vet
```

## Documentation contract

`docs/*-design.md` captures **architecture and design decisions**, not a mirror of the
codebase. Each covers one package boundary or cross-cutting concern and records the **why**
behind how things are shaped: ownership rules, cache-stability requirements, ordering
guarantees, precedence. Read these as invariants that must hold when you change code — read
the doc for a package before working in it, treat its stated constraints as tests to satisfy,
and update it only when your change alters one of those decisions or adds a new architectural
one. Implementation details (function bodies, struct fields, wiring) belong in the code and its
comments, not here.

The docs build on each other: `agent-loop-design.md` is the core (tools,
sessions and compaction all depend on it), and several reference the prompt surfaces
collected in `prompt-design.md`. When a change crosses boundaries, read — and if
needed update — every document that names the affected package.

The README carries the other half of the contract: what **new users** must know to
get started. It covers model setup (`~/.ajent/models.json`), the important
`config.json` options, common flags, and general patterns (like slash commands) that
make everything else discoverable on their own. It highlights what a newcomer needs,
not every function or feature — those live in `--help`, `/commands`, and the design
docs. Update it when you introduce something new users need to find; don't grow it
into a reference mirroring either surface.

| Document | Package(s) / scope | What it contains |
|---|---|---|
| `agent-loop-design.md` | `pkg/agent` | The turn loop (prompt → stream → tool-call → repeat), single-owner `State`, the event sink front ends adapt onto, interruption as a first-class operation. Reference for how tools, sessions and compaction build on it. |
| `providers-design.md` | `pkg/llm` (+ `pkg/config` paths) | One streaming interface over five vendors; the content model, event stream, normalisation pass (`Prepare`), capabilities vs special cases, discovery, retry. Rules for wire structs and what must never leak upward. |
| `tools-design.md` | `pkg/tools` (+ `pkg/agent.Tool`) | The `Tool` interface and registry, built-in tools (incl. the four read-only git readers on go-git), and shared infra: path policy, read tracking, output limits, the guard chain. |
| `session-design.md` | `pkg/session`, CLI resume | Append-only JSONL transcript as source of truth; entry/parent tree, branching, rewind/fork, resume (`--resume`, `--continue`, `--session <name>`) and named sessions, deletion (`--delete`, `--delete-old` via `DeleteSession`/`DeleteOldSessions`). Schema and replay rules. |
| `compaction-design.md` | `pkg/compact` (+ session) | Verbatim-band + checkpoint model: keep the most recent steps verbatim, fold everything older into one structured summary recorded on a compaction entry and replayed. Free structural reduction shapes only the summariser's transcript. Uses `prompt-design.md` summaries. |
| `command-design.md` | `pkg/command`, `pkg/refs`, TUI overlay | Dispatch of every non-prompt line: slash-command registry (open to MCP), direct `!` shell execution via the stager (`!!` runs excluded from context), `@`-path expansion with auto-read and gitignore-aware completion. |
| `tui-design.md` | `pkg/tui` | Render modes, the paint layer, interaction rules; goals in priority order (scrollback survival, minimal chrome, correct formatting) that drive every hard decision. No external TUI framework. |
| `mcp-design.md` | `pkg/mcp` (+ registry states in `pkg/tools`, `/mcp` in `pkg/command`, TUI group rows) | The MCP client and server manager: config merge of `mcp.json`, transports, the bridge that turns remote tools into `agent.Tool`, lifecycle (startup modes, reconnect), deferred loading. Boundary rules for keeping mcp-go isolated to `pkg/mcp`. |
| `config-design.md` | `pkg/config` | Layered loading with per-key provenance and precedence (default → user → project → local), schema-derived environment binding, session overrides that survive resume, the ordered writer, secrets handling (`apiKey`) rules. |
| `clipboard-copy-feature.md` | `pkg/clipboard` (+ `pkg/llm`, `pkg/session`, `pkg/command`, TUI control in `pkg/tui`, routing in `pkg/app`) | The shared clipboard writer (platform-native first, OSC 52 only on remote sessions) and its two surfaces: `/copy` for the last agent response and `ctrl+x` in the rewind picker. Tool call/result JSON copies verbatim, never as display labels. |
| `subagents-design.md` | `pkg/subagent` (+ seams in `pkg/agent`, `pkg/tokens`, `pkg/config`) | Fan-out of read-only investigation into throwaway child agents: the structural tool filter (never `agent_*`, never shell), activity-row sink, bounded concurrency and per-job cancellation, completion notification with delivery confirmation (`Input.Delivered`), child spend accounting. Boundary rules keep it decoupled from tools/tui/command via narrow interfaces supplied by pkg/app. |
| `plan-design.md` | `pkg/plan` (+ seams in `pkg/agent`, `pkg/session`, `pkg/tools`) | The two-model `/plan` workflow: phases as branches of the session tree rather than projections of one message list, the `Host` boundary, the user gate on the drafted plan, per-phase model and tool scope with guaranteed restore, `dev_*` control tools and `ToolResult.EndTurn`, persistence and resume. |
| `prompt-design.md` | every string sent to a model | Each prompt surface ajent sends; the principles enforced by tests: cache-stability of the system block, cheap/stable/honest prompts, provenance markers on all injected content. The single reference for prompting. |

## Architecture

Dependency direction is load-bearing. Actual internal edges:

```
config   (no internal deps — paths, JSON merge, layered settings)
strutil  (no internal deps — tiny shared string helpers)
httputil (no internal deps — the hardened outbound HTTP client)
clipboard(no internal deps — the shared clipboard text writer)
version  -> config, httputil
llm      -> config, strutil, httputil, version
tokens   -> llm
agent    -> llm, tokens
tools    -> agent, config, llm, strutil
session  -> agent, config, llm, strutil, tokens, tools
compact  -> llm, session, strutil, tokens, tools
tui      -> img, strutil (+ goldmark, uniseg, go-udiff, chroma)
mcp      -> agent, config, llm, strutil, version (+ mcp-go; never tools/tui/command — adapters live in pkg/app)
subagent -> agent, llm, strutil, tokens (never tools/tui/command/session/permit — ToolSource + func Options supplied by pkg/app)
plan     -> agent, llm, strutil (never tools/tui/command/session — the driver supplies Host)
projinit -> agent, llm, tools (never tui/command/session/subagent — /init drives the
            real read and agent_* tools through the registry)
permit   -> agent, tools, strutil (never tui; prompter/classifier interfaces are supplied by pkg/app)
refs     -> agent, llm, tokens, tools, tui
command  -> agent, config, llm, refs, tokens, tools, tui, version
app      -> everything except httputil; nothing imports it (the only wiring layer); root
            main() parses flags, short-circuits --version/--update/--delete/--delete-old,
            and calls app.Run / app.RunDelete / app.CheckSessionTarget
```

### Outbound HTTP (`pkg/httputil`)

Every outbound request goes through this one hardened leaf package; it knows nothing
about providers and owns all hardening (no redirects, env proxies, explicit pool bounds,
credential redaction). MCP traffic is not on this client — mcp-go owns its own transports.

### The turn loop (`pkg/agent`)

`Agent` owns one `State`, owned by the goroutine running the turn. Input is single-owner:
mid-turn steering and follow-up are separate paths, never a second concurrent prompt.

Invariants worth memorising:

- Assembling messages from state is **pure** — compaction and plan projection transform
  the assembled list, never `State`.
- The system block stays **cache-stable** across requests in a session (only day-granular
  date and project-instruction reloads may differ).
- On abort every unanswered tool call gets an error result; a dangling one breaks the next
  request permanently. Tool errors are results appended in **call order**, not Go errors.
- Parallel dispatch only when every call allows it and the model supports it.

### Providers (`pkg/llm`)

One streaming interface over five vendors (three ride one compat shim). Differences that
can't be normalised are declared as `Capabilities`, never leaked upward. A single
normalisation pass means what is counted is what is sent.

### Sessions and compaction

The transcript is the source of truth: append-only JSONL forming a **tree** via parent ids,
never deleted — rewinding forks from an earlier point. Compaction folds everything before a
verbatim band into one checkpoint recorded on a `compaction` entry and replayed on every
rebuild; only the newest applies, so each run recomputes cumulatively.

### Front end and dispatch

`pkg/app` is the only wiring layer (nothing imports it). It classifies each line as prompt /
`/command` / `!shell`, feeds ordering to a single **prompt pump** goroutine, and sends shell
lines straight to a non-blocking stager. A one-shot (`-p`) run wires the same loop onto a
stdout drain instead of the TUI — its safety model is the tool set (gate at allow-all, scope
flags decide what's offered), not the permission barrier. Exit codes are `app.ExitOK`/`ExitUsage`/
`ExitTurn`: 0 answer, 1 usage/setup error, 2 failed turn.

### Permission barrier (`pkg/permit`)

The tool gate classifies every call and prompts for approval; it imports only agent/tools,
never tui, so headless stays free. Only **verifiably read-only** actions run without
approval: built-in readers by name, declared-read-only tools (MCP hint / config globs),
bash through a quote-aware analyser. Network commands are never read-only.

## Package map

Quick navigation from concern → package (read the matching design doc first). One line each.

- **`pkg/agent`** — turn loop: `Agent`, `State` (single owner), event sink, pure message assembly, cache-stable system block. Everything else builds on it.
- **`pkg/app`** — the only wiring layer; nothing imports it. Prompt pump + line dispatch, headless (`-p`) run, and thin adapters that supply seams other packages refuse to import (permit classifier, compact driver, plan Host).
- **`pkg/clipboard`** — shared clipboard writer: platform-native first, OSC 52 on remote only.
- **`pkg/command`** — `/slash`, `!shell`, and prompt-line classification + completion; registry open to MCP. Uses `pkg/refs` for `@` expansion.
- **`pkg/compact`** — compaction: verbatim band, cut point, structured summary checkpoint recorded on a session entry.
- **`pkg/config`** — layered settings with per-key provenance; ordered writer and secret-perm handling.
- **`pkg/httputil`** — the one hardened outbound HTTP client (no redirects, retries, credential redaction). MCP traffic does not ride it.
- **`pkg/img`** — image normalization for model input: sniff, downscale/convert under a ceiling.
- **`pkg/llm`** — one streaming interface over five vendors; normalisation pass (`Prepare`), capabilities, registry/discovery from `models.json`. One file per vendor.
- **`pkg/mcp`** — MCP client/server manager keeping mcp-go isolated here; bridge turns remote tools into `agent.Tool`.
- **`pkg/permit`** — permission barrier: classify every tool call, prompt for approval unless verifiably read-only (bash via quote-aware analyser).
- **`pkg/plan`** — `/plan`: two-model phases as session-tree branches; supplies `dev_*` control tools.
- **`pkg/projinit`** — `/init`: scaffolds project instructions into the repo by driving real read and agent_* tools.
- **`pkg/refs`** — `@`-path expansion with auto-read and gitignore-aware completion index.
- **`pkg/session`** — append-only JSONL transcript as source of truth: entry tree, rewind/branch/resume, replay to a sink, compaction data.
- **`pkg/strutil`** — tiny shared string helpers (Clip, FirstLine, HumanSize...). Leaf package.
- **`pkg/subagent`** — fan-out child agents for read-only investigation; bounded concurrency, completion notification, spend rollup. Never runs shell or agent_* tools.
- **`pkg/tokens`** — token estimation and spend accounting (child spend rolls into parent ledger).
- **`pkg/tools`** — tool registry + built-ins (read/write/edit/bash/grep/find/diff/ask), guard chain, path policy, per-tool limits.
- **`pkg/tui`** — no-framework terminal UI: paint layers, scrollback survival, markdown/highlight rendering.
- **`pkg/version`** — version string + self-update.

## Code Style

- Use `var` style for zero-value initialization: `var foo bool`, not `foo := false`. Applies to every type.
- Comments are concise short phrases, not full sentences, and only where they add non-obvious context — never restating a single line of code.
- Wrap comments at ~100 columns.
- Godocs describe inputs and outputs, not how the function works.
- Follow existing naming conventions and neighboring code style.

**Collection handling** — reach for stdlib `slices`/`maps`/`strings` and `github.com/go-analyze/bulk` before a manual loop:

- Clone whole slice/map: `slices.Clone(src)` / `maps.Clone(src)` — not `make`+`copy` (`copy` is still correct for sub-slice writes into an existing buffer).
- Filter (same element type): `bulk.SliceFilter(pred, s)`, or `bulk.SliceFilterInPlace` when the input backing array isn't reused.
- Slice → set: `bulk.SliceToSet(s)` (`map[T]struct{}`), test with `if _, ok := set[k]; ok`. `bulk.SliceToSetBy` for a key func (see `sidescale/dispatch.go`).
- Map → keys/values slice: `bulk.MapKeysSlice(m)` / `bulk.MapValuesSlice(m)` — not a `for k := range m` append loop.
- Membership: `slices.Contains` (comparable) / `slices.ContainsFunc` (predicate).
- Custom sort: `slices.SortFunc` / `slices.SortStableFunc`, not `sort.Slice{,Stable}`.

## Testing

Structure and conventions:
- One `_test.go` file per implementation file that requires testing.
- Test functions have no godocs.
- One `func Test<FunctionName>` per target function, using table-driven tests or `t.Run` cases.
- Test case names are at most 3–5 words, lower case with underscores.
- `t.Parallel()` at test-function start when there's no shared state, but not in the individual cases.
- Isolated temp dirs via `t.TempDir()`; context timeouts via `t.Context()` for tests with I/O.
- Cleanup via `t.Cleanup`, not `defer`.

Assertions and validation:
- `testify`: `require` for setup, `assert` for assertions.
- No assertion messages unless the message adds context beyond the test point itself.
- Never `time.Sleep` — use `require.Eventually` or a deterministic trigger.
- Check every returned error with `require.NoError` / `assert.NoError` whenever `*testing.T` is in scope, except inside `t.Cleanup` and goroutines.
- Verify with `make test-all` and `make lint` before considering a change complete.

