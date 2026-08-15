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

`docs/*-design.md` is the specification for what is built, not notes. Each covers one
package boundary or cross-cutting concern: it states **why** the code is shaped as it
is and records the invariants that must hold when you change it (ownership rules,
cache-stability requirements, ordering guarantees, precedence). These are not prose
to skim — they encode decisions that would otherwise be lost. Read the document for a
package before working in it, treat its stated constraints as tests to satisfy, and
**update the document in the same change whenever you alter one of those behaviours**,
so the spec never drifts from the code.

The documents build on each other: `agent-loop-design.md` is the core (tools,
sessions and compaction all depend on it), and several reference the prompt surfaces
collected in `prompt-design.md`. When a change crosses boundaries, read — and if
needed update — every document that names the affected package.

| Document | Package(s) / scope | What it contains |
|---|---|---|
| `agent-loop-design.md` | `pkg/agent` | The turn loop (prompt → stream → tool-call → repeat), single-owner `State`, the event sink front ends adapt onto, interruption as a first-class operation. Reference for how tools, sessions and compaction build on it. |
| `providers-design.md` | `pkg/llm` (+ `pkg/config` paths) | One streaming interface over five vendors; the content model, event stream, normalisation pass (`Prepare`), capabilities vs special cases, discovery, retry. Rules for wire structs and what must never leak upward. |
| `tools-design.md` | `pkg/tools` (+ `pkg/agent.Tool`) | The `Tool` interface and registry, built-in tools, and shared infra: path policy, read tracking, output limits, the guard chain. |
| `session-design.md` | `pkg/session`, `cmd/ajent` resume | Append-only JSONL transcript as source of truth; entry/parent tree, branching, rewind/fork, resume (`--resume`, `--continue`). Schema and replay rules. |
| `compaction-design.md` | `pkg/compact` (+ session) | Staged context reduction: cheap structural cuts first (failed/superseded calls, elision), an LLM summary only when those are insufficient; the measured `Reduce` plan recorded on a compaction entry and replayed. Uses `prompt-design.md` summaries. |
| `command-design.md` | `pkg/command`, `pkg/refs`, TUI overlay | Dispatch of every non-prompt line: slash-command registry (open to extensions/MCP), direct `!` shell execution via the stager, `@`-path expansion with auto-read and gitignore-aware completion. |
| `tui-design.md` | `pkg/tui` | Render modes, the paint layer, interaction rules; goals in priority order (scrollback survival, minimal chrome, correct formatting) that drive every hard decision. No external TUI framework. |
| `mcp-design.md` | `pkg/mcp` (+ registry states in `pkg/tools`, `/mcp` in `pkg/command`, TUI group rows) | The MCP client and server manager: config merge of `mcp.json`, transports, the bridge that turns remote tools into `agent.Tool`, lifecycle (startup modes, reconnect), deferred loading. Boundary rules for keeping mcp-go isolated to `pkg/mcp`. Reference phase 11's extension protocol builds on this client. |
| `config-design.md` | `pkg/config` | Layered loading with per-key provenance and precedence (default → user → project → local), schema-derived environment binding, session overrides that survive resume, the ordered writer, secrets handling (`apiKey`) rules. |
| `prompt-design.md` | every string sent to a model | Each prompt surface ajent sends; the principles enforced by tests: cache-stability of the system block, cheap/stable/honest prompts, provenance markers on all injected content. The single reference for prompting. |

## Architecture

Dependency direction is load-bearing. Actual internal edges:

```
config   (no internal deps — paths, JSON merge, layered settings)
llm      -> config
tokens   -> llm
agent    -> llm, tokens
tools    -> agent, config, llm
session  -> agent, config, llm, tokens, tools
compact  -> llm, session, tokens, tools
tui      (no internal deps)
mcp      -> agent, config, llm (+ mcp-go; never tools/tui/command — adapters live in main.go)
refs     -> agent, llm, tokens, tools, tui
command  -> agent, config, llm, refs, tokens, tools, tui
main.go  -> everything (the only wiring layer)
```

### The turn loop (`pkg/agent`)

`Agent` owns one `State` (messages, model, reasoning, enabled tool names). `State` is
owned by the goroutine running the turn; only the input queue and `running` flag sit
under the mutex. `Prompt` is single-owner — mid-turn input goes through `Steer`
(injected at the next step boundary) or `FollowUp` (a separate later turn), never a
second concurrent `Prompt`.

Invariants worth memorising:

- `assemble(state, transform)` is **pure** — compaction and plan projection transform
  the assembled list, never `State`.
- `buildSystem` must stay **cache-stable**: only day-granular date, project-instruction
  reloads and tool-set changes may differ between requests in a session.
- On abort, every unanswered `ToolCallBlock` gets a synthetic error `ToolResultBlock`.
  A dangling `tool_use` makes the next Anthropic request 400 permanently.
- Tool errors are `ToolResultBlock{IsError:true}` results, not Go errors — the turn
  continues. Results are appended in **call order** regardless of completion order.
- Parallel dispatch only when every call is `ModeParallel` and `Model.Caps.ParallelTools`.

### Providers (`pkg/llm`)

One streaming interface over anthropic, openai, openrouter, llama.cpp, lm-studio.
Three of the five are a profile over `openaicompat.go`. Differences that cannot be
normalised are declared as `Capabilities`, never leaked upward as special cases.

- `*_wire.go` files hold JSON structs only, no logic.
- `Prepare` is the single normalization pass — every `build*Body`, the estimator and
  the exact counter go through it, so what is counted is what is sent.
- Blocks are stored as values (immutable once appended); `BlockList` carries a
  `{type,data}` envelope so transcripts round-trip.
- `ThinkingBlock` carries every provider's replay token at once; `Redacted` stays a
  `string` because a base64 round-trip must be byte-exact.
- Wire-format fixtures live in `pkg/llm/testdata/<provider>/`; `llm.ScriptedProvider`
  (`fake.go`) is the provider stub for tests anywhere in the tree.

### Sessions and compaction

The transcript is the source of truth: append-only JSONL, one `Entry` per line, each
naming its `ParentID` so the file is a **tree**, not a log. `Branch(entries, id)` is
the only read path. Nothing is ever deleted — rewinding sets `HEAD` to an earlier
entry's parent and forks.

Because of that, compaction cannot just rewrite the in-memory message list — it records
a `Reduce` plan (stubs, drops, strip-thinking) on the `compaction` entry, replayed on
every rebuild. `pkg/session` owns the schema and replay; `pkg/compact` computes the
plan and measures each stage through the same `session.ContextMessages`, so a measured
saving is by construction the saving the next request gets. Only the newest compaction
applies, so each run recomputes cumulatively over the whole branch.

### Front end and dispatch

`main.go` classifies each submitted line with `command.ParseLine` (prompt / `/command`
/ `!shell`) and feeds commands and prompts to a single **prompt pump** goroutine that
owns ordering; shell lines go straight to the non-blocking `Stager`. Prompts flush the
stage, expand `@` refs, then steer or start a turn.

`demo.go` (behind the `demo` build tag) is a scripted stand-in for the real driver,
used to exercise every rendering path without a provider; `run_agent.go` carries the
`!demo` counterpart. Both define `drive(...)` with the same signature.

## Code Style

- Use `var` style for zero-value initialization: `var foo bool`, not `foo := false`.
- Comments are concise short phrases, not full sentences, and only where they add non-obvious context — never restating a single line of code.
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

