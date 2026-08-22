# Configuration design

How `pkg/config` answers "where does this value come from", and how `/settings`
changes it. It is the single configuration system for ajent: layered loading
with per-key provenance, a schema-derived environment binding, session
overrides that survive resume, and an ordered writer that preserves unknown keys.

## Layers

Configuration resolves lowest to highest precedence:

1. **default** — compiled in (`config.Defaults()`), kept as a JSON literal so it
   reports `(default)` like any other source.
2. **user** — `~/.ajent/config.json`. May hold a literal `apiKey` under
   `providers`, with a loud warning if the file is group- or world-readable.
3. **project** — `<workspace>/.ajent/config.json`, committed at the team's
   discretion. The loader refuses a literal `providers.*.apiKey` here and warns,
   because this file gets checked in.
4. **local** — `<workspace>/.ajent/config.local.json`. Same apiKey refusal; it is
   gitignored but still shared by convention, so the same rule applies.
5. **env** — `AJENT_*` variables, derived by reflection from the schema: a scalar
   key at dotted path `p.q.r` binds to `AJENT_P_Q_R`. An unparseable number or
   bool is a warning, never fatal.
6. **flag** — built by the caller with `config.SetKey` over `{}`.
7. **session** — what `/settings`, `/model`, `/reasoning` and `/tools` change at
   runtime; empty at startup.

Merge is per-key: objects fold deeply, arrays and scalars replace wholesale.
`Resolved.Explain(key)` returns the resolved value plus the layer that supplied
it — the difference between a config system and a mystery.

## The schema

```go
type Settings struct {
    Model       string          // llm model key; the default a fresh start uses
    Reasoning   Reasoning       // level/retain as text names, parsed by caller
    Agent       Agent           // turn loop: optional per-turn step cap (maxSteps)
    Providers   json.RawMessage // folded over models.json providers (pkg/llm)
    Models      json.RawMessage // "provider/id" overrides (pkg/llm)
    Tools       Tools           // enabled set + per-tool output limits
    Permissions Permissions     // mode + safeCommands; enforced by the tool barrier (permit)
    Compaction  Compaction      // auto toggle + threshold fraction
    Subagent    Subagent        // research sub-agent model + concurrency cap
    UI          UI              // render mode + color palette; showCost/showThinking
    Extensions  Extensions      // loaded by the extension host, accepted now
}
```

Enum-valued keys are stored as their text names and parsed by the caller
(`llm.ParseLevel`, `llm.ParseRetain`, `tui.ParseMode`, `permit.ParseMode`).

### Model

`model` is the model a fresh start defaults to, resolved through normal layer
precedence and handed to the registry before discovery runs. Every `/model`
change (and the `/settings` Model row, which routes through the same command)
writes the selection to the **user** layer in addition to the session override,
so the next start keeps the most recent choice — there is no separate
last-used key. Because the write lands in the user layer, a `model` pinned in
project/local config or an `-m` flag still outranks it, and a resumed session
replays its own `model_change` entries instead. A failed save is a warning,
never a lost switch.

### Permissions

The permission block defaults to `{"mode": "allow-read"}`, so
`Explain("permissions.mode")` resolves and reports `(default)`. The mode name is
one of the five in the barrier (`allow-all`, `allow-read`, `auto`, `auto+mcp`,
`block-all`); `AJENT_PERMISSIONS_MODE` binds for free
through EnvLayer. It seeds a session's live barrier at startup, so a resumed
session restores its cycled mode (rebuild replays session overrides before this).
A `Shift+Tab` cycle or `/settings` records the change as a **session** override
via `SetSessionSetting("permissions.mode", …)` — never rewriting the config file.
`/settings`'s Permissions row edits the persistent default instead, offering save
to user/project layer like any other enum row.

`safeCommands` lists exact MCP/extension tool names or bash command lines that
auto-allow as read-only in allow-read/auto (and auto+mcp). A single shell entry matches at a token
boundary, so `git` covers every git invocation and `git status` its subcommands; a
compound line (`cd … && make lint | tail`) instead requires **every** component to be
either a listed entry or verifiably read-only — wrapping in `cd`/pipe never defeats
the match, and an appended write can't ride in on a listed prefix. It can never name
a core writer (`write`, `edit`) or un-reject an in-place sed, so no config entry
overrides a known mutation. It gates on the live call's tool name (exact) or bash
components (see permit), independent of registry metadata.

`deniedCommands` is its hard inverse: exact tool names, whole MCP server namespaces,
or bash command lines that are always refused **without prompting** in every mode —
including allow-all. Matching follows the same token-boundary rule as `safeCommands`;
a compound line is refused when *any* component matches, so nesting a denied
command behind `cd … &&` never escapes it. It may also name core writers, since
denying one is a legitimate safety gate. A denied check runs first in the barrier
verdict (after user-initiation), and only an agent call hits it: a human's own staged
`!` line owns its shell and always runs.

### Subagent

The sub-agent block ships a compiled-in `maxConcurrent` default (it lives in
`defaultsJSON`, beside this package); `model` is deliberately
left out so `Explain("subagent.model")` reports `(default)` and an empty value
means inherit the session model. Both keys bind for free through EnvLayer's
reflection (`AJENT_SUBAGENT_MODEL`, `AJENT_SUBAGENT_MAXCONCURRENT`) and are edited
from `/settings`. Per `## The rule` below, `subagent.model` is a plain string key,
resolved against the model registry by the caller — never an llm import here.

`providers`/`models` stay raw because **`pkg/config` must never import `pkg/llm`** — `pkg/llm`
imports it for paths, and a typed reference would cycle. `models.json` is decoded
in `pkg/llm`, and config's provider/model blocks fold over it via
`llm.ApplyOverrides`.

### Agent

The agent block holds `maxSteps`, an **optional** cap on one turn's tool-calling
iterations. Like `subagent.model`, it is deliberately absent from the defaults
layer, so `Explain("agent.maxSteps")` reports `(default)` and an empty value
means **unlimited** — the zero value of `agent.Options.MaxSteps` (see
agent-loop-design.md). `AJENT_AGENT_MAXSTEPS` binds for free through EnvLayer;
a positive value caps the turn, any non-positive value (or none) leaves it
uncapped. It is startup-time configuration: main.go copies it into
`agent.Options.MaxSteps` once at process start, so it is deliberately absent
from `/settings`, whose session overrides could never reach the running agent.

### UI

`ui.render` is a `tui.Mode` name defaulting to `"auto"`, read once before
`tui.New` — a `/settings` override could never reach the live renderer, so, like
`agent.maxSteps`, it has no row.

`ui.theme` is a `tui.Palette` name (`dark`, `light` and their `-cool`, `-warm`,
`-muted` variants) defaulting to `"dark"`. `AJENT_UI_THEME` binds for free
through EnvLayer. The default is what makes the first-run picker possible:
`command.ThemeSetup` opens only while `Source("ui.theme")` still reports
`default`, so a value in any layer — env, project, local or a previous answer
saved to user — suppresses it. Picking (or dismissing) writes the name to the
**user** layer and records a session override, so a project pin still outranks
it. Unlike `ui.render` the palette *can* change at runtime: `/settings → Theme`
recolors the live UI, and a resumed session applies its override before the
transcript replays (see tui-design.md, "Semantic styling").

## The writer

Saving re-marshals an order-preserving object tree at two-space indent: unknown
keys and key order survive, formatting is normalized, comments are dropped. When
the target file already carries `//` comments the save warns that they will be
lost. Writes go through `WriteFileAtomic` with secret permissions.

## Secrets

- API keys live in the environment or a literal `apiKey` in the **user** layer.
- A project or local file with `providers.*.apiKey` is stripped before merging,
  with one loud warning per removed key — those files get committed.
- Any user file holding a literal apiKey triggers the `0600` check.

## The rule

`pkg/config ↛ pkg/llm`, always. Cross-schema folding happens in `pkg/llm`
(`overrides.go`) and the typed surface stays stdlib-only here.

