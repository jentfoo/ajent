# Configuration design

How `pkg/config` answers "where does this value come from", and how `/settings`
changes it. It is the single configuration system for ajent: layered loading
with per-key provenance, a schema-derived environment binding, session overrides
that survive resume, and an ordered writer that preserves unknown keys.

## Layers

Configuration resolves lowest to highest precedence:

1. **default** — compiled in (`config.Defaults()`), kept as a JSON literal so it
   reports `(default)` like any other source.
2. **user** — the user's `~/.ajent/config.json`.
3. **project** — `<workspace>/.ajent/config.json`, committed at the team's discretion.
4. **local** — `<workspace>/.ajent/config.local.json`, never committed; overrides
   project for machine-specific values.
5. **env** — bound from `AJENT_*` variables, one per scalar key (a dotted path
   `p.q.r` binds to `AJENT_P_Q_R`). An unparseable number or boolean warns and
   keeps the lower layer rather than failing startup.
6. **flag** — set by the caller at startup over an empty base. Only `model`
   and `ui.render` ride it; the one-shot flags deliberately do not (see below).
7. **session** — per-key session overrides that survive resume, recorded from
   `/settings` and runtime mode cycles; empty at a fresh start.
Merge is per-key: objects fold deeply, arrays and scalars replace wholesale.
`Resolved.Explain(key)` returns the resolved value plus the layer that supplied
it. That is the difference between a config system and a mystery.

A `Set` is safe for concurrent use: settings are read while a turn runs (the
prompt pump, mid-turn compaction) and written from key handlers on another
goroutine (a permission mode cycle never waits for the turn). A mutex
serializes layer writes and merge; each merged `Resolved` is immutable once
built, so readers hold a stable snapshot across later writes.

## The schema

The schema is a single typed settings root whose fields mirror the config
blocks: the default model key, reasoning level/retain as text names, the agent
turn-loop options (an optional per-turn step cap), and typed blocks for tools,
permissions, compaction, sub-agent settings, and UI render/palette.

Enum-valued keys are stored as their text names and parsed by the caller, each
package parsing its own.

### Model

`model` is the model a fresh start defaults to, resolved through normal layer
precedence and handed to the registry before discovery runs. A `/model` change
writes the selection to the **user** layer in addition to the session override,
so the next start keeps the most recent choice; there is no separate last-used
key. The `/settings` Model row routes through the same picker but defers its
persistence to the save-to-layer prompt, so a "this session only" answer leaves
the config files untouched like every other settings row. Because the write
lands in the user layer when chosen, a `model` pinned in project/local config or
an `-m` flag still outranks it, and a resumed session replays its own
`model_change` entries instead. A failed save is a warning, never a lost switch.

### Permissions

The permission block has a compiled-in default mode, so `Explain` on it resolves
and reports `(default)`. The mode name is one of the barrier's modes (see
`permit`); `AJENT_PERMISSIONS_MODE` binds for free through EnvLayer. It seeds a
session's live barrier at startup, so a resumed session restores its cycled mode
(rebuild replays session overrides before this). A `Shift+Tab` or `Shift+←/→`
cycle, or `/settings`, records the change as a **session** override via
`SetSessionSetting("permissions.mode", …)`, never rewriting the config file.
`/settings`'s Permissions row edits the persistent default instead, offering
save to user/project layer like any other enum row.

`safeCommands` lists exact MCP/extension tool names or bash command lines that
auto-allow as read-only in allow-read/auto. A single shell entry matches at a
token boundary, so `git` covers every git invocation and `git status` its
subcommands; a compound line (`cd … && make lint | tail`) instead requires
**every** component to be either a listed entry or verifiably read-only.
Wrapping in `cd`/pipe never defeats the match, and an appended write can't ride
in on a listed prefix. It can never name a core writer (`write`, `edit`) or
un-reject an in-place sed, so no config entry overrides a known mutation.
`auto+write` is the only mode that runs a core writer without a prompt, and it
does so on its own path-scope check rather than this list. It gates on the live
call's tool name (exact) or bash components (see permit), independent of
registry metadata.

`deniedCommands` is its hard inverse: exact tool names, whole MCP server
namespaces, or bash command lines that are always refused **without prompting**
in every mode, including allow-all. Matching follows the same token-boundary
rule as `safeCommands`; a compound line is refused when *any* component matches,
so nesting a denied command behind `cd … &&` never escapes it. It may also name
core writers, since denying one is a legitimate safety gate. A denied check runs
first in the barrier verdict (after user-initiation), and only an agent call
hits it: a human's own staged `!` line owns its shell and always runs.

### Permissions and tools in a one-shot run

`-p` does not write any permission or tool key into the flag layer. Its
`--allow-all` / `--read-only` / `--allow-tools` / `--deny-tools` flags choose
the *offered tool set* rather than a gate, so there is no key for them to set
and `Explain` keeps reporting the file's own values. A headless run therefore:

- ignores `permissions.mode` and runs the barrier at `allow-all`, since no dialog
  can be opened; the scope flags are what limit what the model may call, so a
  narrower set is expressed as offered tools rather than a permission change.

The flag surface itself lives in `flags.go`; per the README contract every scope
flag also has its entry there. See `tools-design.md` "Headless: the tool set is
the gate" for the rule.

### Tools

`tools.enabled` replaces the default enabled set (`read`, `write`, `edit`,
`bash`) and `tools.limits` bounds each tool's output.

`tools.shellCommands` names extra shell commands the bash tool description
may advertise. The list starts from a built-in set, adds these extras, then
drops any entry that is not a bare executable name on PATH, refused by
`permissions.deniedCommands`, or already covered by an enabled tool (a rule
naming a subcommand also drops its bare parent). The probe runs once at
startup and warns when an entry is missing from PATH. Like every slice key it
has no `AJENT_*` binding.

### Subagent

The `subagent` block ships a compiled-in `maxConcurrent` default; `model` is
deliberately left out so `Explain` on it reports `(default)` and an empty value
means inherit the session model. Both keys bind for free through EnvLayer's
reflection (`AJENT_SUBAGENT_MODEL`, `AJENT_SUBAGENT_MAXCONCURRENT`) and are
edited from `/settings`. Per `## The rule` below, `subagent.model` is a plain
string key, resolved against the model registry by the caller, never an llm
import here.

### Agent

The agent block holds `maxSteps`, an **optional** cap on one turn's tool-calling
iterations, and `turnRetries`, how often a failed model call is re-requested
within a step (a permanent failure never retries, and `0` keeps the default rather
than disabling). Both are deliberately absent from the defaults
layer, so `Explain` reports `(default)`; `AJENT_AGENT_MAXSTEPS` and
`AJENT_AGENT_TURNRETRIES` bind for free through EnvLayer. They are startup-time
configuration: pkg/app copies them into `agent.Options` once at process start,
so they are deliberately absent from `/settings`, whose session overrides could
never reach the running agent.

The block also holds `systemPrompt`: when set it replaces ajent's default prose
guidance (the opening sentence and guideline bullets) in the system prompt. It is
startup-time configuration like its siblings — pkg/app copies it into
`agent.Options.SystemPrompt`, so a `/settings` override could never reach a live
turn — and `AJENT_AGENT_SYSTEM_PROMPT` binds for free through EnvLayer, with the
command-line flag `--system` outranking both via the flag layer. It has no row in
`/settings`: like `maxSteps`, it is set only by config files, env or the flag,
never at runtime.

### UI

`ui.render` is a `tui.Mode` name defaulting to `"auto"`, read once before
`tui.New`. A `/settings` override could never reach the live renderer, so, like
`agent.maxSteps`, it has no row.

`ui.color` is a `tui.ColorProfile` name defaulting to `"auto"`, read once beside
`ui.render` and, like it, absent from `/settings`: the theme is built before
`tui.New` returns. `auto` detects from `TERM` and `COLORTERM`; any other value
names the depth outright, which is the escape hatch for a terminal we classify
badly in either direction; `AJENT_UI_COLOR=none` is the per-invocation form. An
unknown name warns and falls back to detection rather than exiting, since a bad
colour name is not worth a failed startup (`ui.render` still exits, because
there is no safe paint mode to guess). `NO_COLOR` is still honoured beneath it
but deliberately undocumented. See tui-design.md, "Semantic styling".

`ui.theme` is a `tui.Palette` name defaulting to `"dark"`. `AJENT_UI_THEME`
binds for free through EnvLayer. The default is what makes the first-run picker
possible: `command.ThemeSetup` opens only while `Source("ui.theme")` still
reports `default`, so a value in any layer (env, project, local or a previous
answer saved to user) suppresses it. Picking (or dismissing) writes the name to
the **user** layer and records a session override, so a project pin still
outranks it. Unlike `ui.render` the palette *can* change at runtime:
`/settings → Theme` recolors the live UI, and a resumed session applies its
override before the transcript replays (see tui-design.md, "Semantic styling").
A first run with nothing configured then continues into the setup wizard; see
providers-design.md, "First-run setup".

`ui.images` names a terminal image protocol (`kitty`, `iterm2`, `none`) and
defaults to detection, forcing the capability the way `ui.color` forces depth.
Like `ui.render` it is read once before `tui.New` with no `/settings` row, and
an unknown name falls back to detection (see tui-design.md, "Terminal images").

### Images

`images.block`, off by default, is the one image delivery key. It applies to the
tool layer at startup, and resume re-applies it after the transcript's overrides
are seeded so a session saved blocked comes back blocked.

## The writer

Saving re-marshals an order-preserving object tree: unknown keys and key order
survive, formatting is normalized, comments are dropped. When the target file
already carries `//` comments the save warns that they will be lost. Writes go
through `WriteFileAtomic` with secret permissions.

## Build version

`pkg/config` owns the exported `Version` var, a build-time value resolved by
precedence: ldflags injection → `debug.ReadBuildInfo` module version (when not
`(devel)`) → `dev`. It is deliberately **not** part of the layered config schema
or `config.json`, so it never appears in settings or overrides.

## Update check cache

Disposable caches live under a cache directory (`config.CachePath`), a sibling
directory to the layered config files. The remote-version check reads and writes
a small cache file there: the fetched version plus the time it was checked and
the time a notice was last shown.

Two TTLs bound it: the fetched version is reused for a while before the tags
endpoint is hit again, and an update notice is shown at most once per interval.
The check only reports for real builds, never `dev`, which is always behind by
design, and compares clean `vX.Y.Z` tags strictly. A failed fetch keeps the
stale cached tag; when it already holds a newer release the notice is still
reported, so going offline past the TTL never hides a known update. Only a
failed check with no cached version to fall back on reports an error. Both
writes are best-effort, so a broken cache directory cannot break startup.

The notice can be switched off entirely with `disableUpdateCheck` (a top-level
Settings bool, env `AJENT_DISABLEUPDATECHECK`). It is read once at startup in
pkg/app and deliberately has no `/settings` row, like `agent.maxSteps`. A
session override could never reach the already-running check.

## The rule

`pkg/config ↛ pkg/llm`, always. Cross-schema folding happens in `pkg/llm`
(`overrides.go`) and the typed surface stays stdlib-only here.
