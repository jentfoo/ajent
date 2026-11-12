# Config

`config.json` holds everything except providers and models (those live in
`models.json`, see `models.md`): which model starts a session, how the agent
behaves, what the barrier allows, and how the UI looks. This file lists every
key with its default, its `AJENT_*` environment binding, and whether an edit
takes effect live or needs a restart.

## Files and layers

Configuration resolves lowest to highest, one key at a time:

| Layer | Source | Who writes it |
|---|---|---|
| default | compiled in (reported as `(default)`) | never |
| user | `~/.ajent/config.json` | `/settings` saves, `/model`, the theme picker |
| project | `<workspace>/.ajent/config.json` | `/settings` save to project. Commit at the team's discretion |
| env | one `AJENT_*` variable per scalar key | your shell |
| flag | `-m/--model`, `--render <mode>`, `--system <text>` | ajent, at startup |
| session | `/settings` edits and mode cycles | the running session, survives resume |

Files set any subset of the keys. A file that omits a key leaves lower layers
in charge. Merge is per-key: objects fold deeply, arrays and scalars replace
wholesale. `AJENT_HOME` moves the whole home directory (`~/.ajent` by default),
it is not a config key.

Both files parse leniently: `//` comments and trailing commas are
accepted, blank or comment-only content behaves as an absent file, unknown,
duplicate or wrong-typed keys warn instead of blocking startup, and a top-level
value that is not a JSON object fails with the file and line named. Key spellings
that differ only in letter case are normalized to each key's declared name.

One secret rule: a literal `apiKey` is honored in the user layer only.
The project layer has any `providers.*.apiKey` stripped with a warning,
because those files end up where other people can read them.

## Env bindings

Every scalar key binds mechanically from its dotted path, preserving case:
`p.q.r` becomes `AJENT_P_Q_R`, so `permissions.mode` is
`AJENT_PERMISSIONS_MODE` and `tools.limits.refInject.lines` is
`AJENT_TOOLS_LIMITS_REFINJECT_LINES`. The rules:

- slice keys (`tools.enabled`, `safeCommands`, the keymap lists) have no binding
- an unparseable number or boolean warns and keeps the lower layer's value, it
  never fails startup
- one key also binds a documented alias: `agent.systemPrompt` answers to both
  `AJENT_AGENT_SYSTEMPROMPT` (mechanical) and `AJENT_AGENT_SYSTEM_PROMPT`. When
  both are set the mechanical spelling wins

An env var outranks every config file, and a flag outranks an env var. To try a
value for one run, env is usually what you want (`AJENT_TOOLS_DIFFECHO=never ajent`).

## Model and reasoning

| Key | Default | Env binding |
|---|---|---|
| `model` | first known model from the registry | `AJENT_MODEL` |

The default model for a fresh start, as a `provider/model` key or any name the
registry resolves. A `/model` change writes your most recent choice to the user
layer, so it survives restarts. A value pinned in project config (or an
`-m` flag) still outranks that write. See `models.md`.

| Key | Default | Env binding |
|---|---|---|
| `reasoning.level` | `medium` | `AJENT_REASONING_LEVEL` |
| `reasoning.retain` | unset, model's own default | `AJENT_REASONING_RETAIN` |
| `reasoning.budget` | none (provider decides) | `AJENT_REASONING_BUDGET` |
| `reasoning.hide` | `false`, thinking streams into the view | `AJENT_REASONING_HIDE` |

`level` names one of the seven reasoning strengths and `retain` how much
thinking is kept when sending history back (`none`, `lastTurn`, `wholeTurn`,
`all`), applied only to what is sent, the transcript always keeps everything.
The level vocabulary, per-model support, and what an unset `retain` resolves
to are in `models.md`.

## Agent turn loop

| Key | Default | Env binding |
|---|---|---|
| `agent.maxSteps` | unlimited | `AJENT_AGENT_MAXSTEPS` |
| `agent.turnRetries` | 4 | `AJENT_AGENT_TURNRETRIES` |
| `agent.systemPrompt` | ajent's own guidance | `AJENT_AGENT_SYSTEMPROMPT`, also `AJENT_AGENT_SYSTEM_PROMPT` |

`maxSteps` optionally caps one turn's tool-calling iterations, absent or zero
means unlimited. `turnRetries` is how many times a failed model call is
re-requested before the turn fails, with backoff: dropped connections,
truncated streams and overloaded providers retry, permanent errors (bad
request, no credentials) never do, and `0` keeps the default rather than
disabling retries.

`systemPrompt` replaces ajent's default prose guidance in the system prompt
(the opening sentence and guideline bullets) when set. Environment facts,
project instructions (`AGENTS.md`) and extension snippets still follow, since
tools depend on them. A trailing newline is trimmed. The `--system <text>` flag
sets it for one run and outranks every file layer.

## Tools

| Key | Default | Env binding |
|---|---|---|
| `tools.enabled` | `["read", "write", "edit", "bash"]` | none (slice) |
| `tools.shellCommands` | none beyond the built-in set | none (slice) |
| `tools.diffEcho` | `validate` | `AJENT_TOOLS_DIFFECHO` |

`enabled` is which built-ins start enabled, toggleable per session with
`/tools`. It replaces the default set rather than adding to it. `shellCommands`
names extra shell commands the `bash` tool description may advertise, alongside
the built-in set (`ls`, `grep`, `find`, `diff`, `wc`). A name is advertised only
if it exists on PATH and is not refused by `permissions.deniedCommands`.

`diffEcho` names when `write`/`edit` results echo a unified diff of the change:
`never`, `validate` (echo what was validated), `overwrite` (`write`'s
displaced-content diff only) or `always`. An unknown value warns and keeps the
default.

### Tool output limits

Each tool's output is truncated per dimension, and an unset dimension is
unbounded. Keys are `tools.limits.<tool>.lines` and `.bytes`:

| Tool | Default lines / bytes | Env binding pattern |
|---|---|---|
| `tools.limits.bash` | 4000 lines / 32768 bytes (32 KB) | `AJENT_TOOLS_LIMITS_BASH_LINES`, `_BYTES` |
| `tools.limits.read` | 2000 lines / 536870912 bytes (512 MB) | `AJENT_TOOLS_LIMITS_READ_*` |
| `tools.limits.find` | 500 / unbounded | `AJENT_TOOLS_LIMITS_FIND_LINES` |
| `tools.limits.grep` | 1000 lines / 131072 bytes (128 KB) | `AJENT_TOOLS_LIMITS_GREP_*` |
| `tools.limits.ls` | 500 / unbounded | `AJENT_TOOLS_LIMITS_LS_LINES` |
| `tools.limits.git` | unbounded (shared by all four `git_*` readers) | `AJENT_TOOLS_LIMITS_GIT_*` |
| `tools.limits.other` | unbounded (MCP and extension tools without their own bound) | `AJENT_TOOLS_LIMITS_OTHER_*` |
| `tools.limits.refInject` | 500 lines / 131072 bytes, per `@path` reference | `AJENT_TOOLS_LIMITS_REFINJECT_*` |
| `tools.limits.refTotal` | unbounded lines / 131072 bytes across all `@path` content | `AJENT_TOOLS_LIMITS_REFTOTAL_*` |

All nine bind mechanically, so `refInject.bytes` is
`AJENT_TOOLS_LIMITS_REFINJECT_BYTES`.

## Permissions

| Key | Default | Env binding |
|---|---|---|
| `permissions.mode` | `allow-read` | `AJENT_PERMISSIONS_MODE` |
| `permissions.headlessMode` | `auto+write` when unset | `AJENT_PERMISSIONS_HEADLESSMODE` |
| `permissions.safeCommands` | empty | none (slice) |
| `permissions.deniedCommands` | empty | none (slice) |

`mode` is the interactive barrier mode: `allow-read`, `auto`, `auto+write`,
`allow-all` or `block-all`. What each auto-runs is described in
`permissions.md`. Shift+Tab cycles it for the session without touching config.

`headlessMode` is the one-shot (`-p`, piped) barrier mode, kept separate from
`mode` because unattended sessions gate so differently. Interactive runs never
read it, and a scope flag beats both it and the default (see `cli.md`).

`safeCommands` lists extra auto-run entries; `deniedCommands` refuses its
entries without prompting in every mode, allow-all included, and may name core
writers as a safety gate. What an entry can be (a tool name, an MCP server
namespace, a bash command line) and how matching behaves are described in
`permissions.md`.

## Compaction

| Key | Default | Env binding |
|---|---|---|
| `compaction.auto` | `true` | `AJENT_COMPACTION_AUTO` |
| `compaction.threshold` | `0.8` | `AJENT_COMPACTION_THRESHOLD` |
| `compaction.minSteps` | `2` | `AJENT_COMPACTION_MINSTEPS` |
| `compaction.verbatimFraction` | `0.1` | `AJENT_COMPACTION_VERBATIMFRACTION` |

Automatic context reduction fires when history reaches `threshold`, a fraction
of the model's window (or an absolute token count, when given as one).
`auto: false` stops the automatic trigger only: `/compact` still works, and an
overflow still recovers. A model that sets its own `compactThreshold` in
`models.json` keeps it, and this `threshold` is the default for those that do
not.

What a step is, how the verbatim band forms around these two keys, and what
the summary replaces are described in `sessions.md`.

## Sub-agents

| Key | Default | Env binding |
|---|---|---|
| `subagent.model` | empty, inherits the session model | `AJENT_SUBAGENT_MODEL` |
| `subagent.maxConcurrent` | 8 | `AJENT_SUBAGENT_MAXCONCURRENT` |
| `subagent.maxPollWait` | `"10m"` | `AJENT_SUBAGENT_MAXPOLLWAIT` |
| `subagent.maxSteps` | 200 | `AJENT_SUBAGENT_MAXSTEPS` |

`model` runs children on a different model key, resolved through the registry
like any other. `maxConcurrent` caps parallel jobs with the rest queued in
order, `maxPollWait` is how long one `agent_poll` blocks before reporting the
job still running (like `"20m"` or `"90s"`), and `maxSteps`
bounds one child's turn so a runaway investigation cannot burn tokens forever.

The last three are read once at startup: an edit reaches a later run, never the
live manager. A `/settings` override of them still persists for that later run.
Behavior and job lifecycle live in `agents.md`.

## UI

| Key | Default | Env binding |
|---|---|---|
| `ui.render` | `auto` | `AJENT_UI_RENDER` |
| `ui.color` | `auto` | `AJENT_UI_COLOR` |
| `ui.theme` | `dark` | `AJENT_UI_THEME` |
| `ui.images` | unset, detect | `AJENT_UI_IMAGES` |
| `ui.keymap.submit` | `["enter"]` | none (slice) |
| `ui.keymap.newline` | `["ctrl+j", "alt+enter"]` | none (slice) |

`render` is the paint mode: `auto`, `inline`, `alt` or `plain`; what each
paints and how detection resolves are in `tui.md`. An unknown value fails
startup, since there is no safe paint mode to guess.

`color` names a color depth: `auto` (detects from `TERM` and `COLORTERM`),
`none`, `basic`, `256` or `true`. Any other value than those names the depth
outright. This is the escape hatch for a terminal classified badly in either
direction (`256` turns on syntax highlighting where detection was too
conservative, `none` turns color off). An unknown name warns and falls back to
detection.

`theme` picks one of the built-in palettes: `dark`, `dark-cool`, `dark-warm`,
`dark-muted`, `light`, `light-cool`, `light-warm`, `light-muted`. The theme
picker on first run writes your choice here, and `/settings → Theme` recolors
the live UI. An unknown name warns and uses the default.

`images` forces a terminal image protocol (`kitty`, `iterm2`) or turns rendering
off (`none`). Unset means detection from the terminal's reported capabilities,
which reports none inside multiplexers like tmux and screen (placeholders are
shown instead). Rendering details in `tui.md`.

The keymap lists are overrides over hard-coded defaults, so an unlisted key
keeps its default. To make Enter insert a newline and Alt+Enter send:
`"keymap": { "submit": ["alt+enter"], "newline": ["enter"] }`. A key bound to
both actions, or an override leaving nothing to submit, warns and falls back to
the defaults at startup.

## Images and control

| Key | Default | Env binding |
|---|---|---|
| `images.block` | `false` | `AJENT_IMAGES_BLOCK` |
| `control.enabled` | `false` | `AJENT_CONTROL_ENABLED` |

`block`, when true, prevents any image block from reaching the model: pastes,
`@file.png` references and tool images all become an
`Image reading is disabled.` text note. Off by default, and `/settings → Block
images` flips it live for the session.

`enabled` exposes the running session on a same-user unix socket so other
programs or terminals can join it, and allows `--serve`, which implies it
whatever config says. It is a deliberate act, off by default, and restart to
change it. Enabling it and joining a served session are described in
`sessions.md`.

## Update check

| Key | Default | Env binding |
|---|---|---|
| `disableUpdateCheck` | `false` | `AJENT_DISABLEUPDATECHECK` |

Turns off the startup update-available notice (its caching rules live in
`cli.md`). Read once at startup, so an edit needs a restart.

## Editing settings

`/settings` lists every key that can be edited safely, each row showing its
current value and which layer supplied it. Saving offers user or project as the
target layer, or "this session only", which holds an override in the session
layer instead of writing any file.

Session overrides survive resume: they are recorded on the transcript and
re-applied when the session comes back, above every file layer.
`permissions.mode` is the one exception, its cycles apply for the run only and
a resumed session starts from the configured (or default) mode. `/settings →
Reset overrides` removes them one at a time or all at once, so lower layers
apply again.

Some keys are copied into objects built during startup and cannot be re-applied
to a running session: everything in the agent turn loop and sub-agent manager,
UI render/color/images/keymap setup, `tools.shellCommands`, `tools.diffEcho`,
the barrier's safe/denied lists, `control.enabled` and `disableUpdateCheck`.
A `/reload` (which re-reads all three files plus env) names any of these that
changed instead of applying them. It adopts everything else live: model,
reasoning rows, compaction bounds, permission mode, theme and image blocking.
The one further exception is `tools.enabled`, which a reload deliberately skips:
it never enables a tool or disables a live one, toggles stay with `/tools`.

## See also

- `models.md` - providers, flavors, discovery, models.json declarations
- `permissions.md` - what each barrier mode auto-runs, dialog choices
- `cli.md` - command-line flags and the headless one-shot contract
- `sessions.md` - compaction mechanics, resume, control socket
- `commands.md` - `/settings`, `/tools`, `/model`, `/reload`
