# CLI

Running ajent from a shell: install and update, first-run onboarding, every
command-line flag with its defaults and interactions, and the headless one-shot
contract (exit codes, `-o json` events, piped stdio, `--stats`). Session
naming/matching/deletion rules live in `sessions.md`, permission modes and how
unattended runs settle asks in `permissions.md`, configuration files and
`AJENT_*` bindings in `config.md`, provider and model setup in `models.md`.

## Install

```sh
go install github.com/jentfoo/ajent@latest
```

Requires a Go toolchain, with `$HOME/go/bin` (or `GOBIN`) on `PATH`. There are
no published binaries. Building from source is the alternative:

```sh
make build   # -> ./bin/ajent
```

Verify with `ajent --version`. Installs through `go install` carry the module
version, while a bare `go build .` reports `dev`.

## Update

- `ajent --update` resolves the latest published version from the Go module
  proxy and reinstalls it with `go install github.com/jentfoo/ajent@latest`,
  in the foreground. It never opens a session. Exit 0 on success (including
  "already up to date") and exit 1 when resolution or install fails. The
  outcome prints as one line.
- `/update` is the same thing from inside an interactive session.

At startup, interactive runs check for a newer release and print one notice:
the fetched tag is cached in `~/.ajent/cache/remote-version.json` and re-fetched
at most every 12 hours, and no more than one notice per 2 hours per install.
Dev builds never nag. Set `"disableUpdateCheck": true` in any config layer to
turn the check off (`config.md`). One-shot, serve and attach runs never check.

## First run

ajent keeps its state under `~/.ajent`, created owner-only on first start.
`AJENT_HOME` overrides that location entirely. Two wizards may greet the first
interactive start:

1. **Theme picker** (skipped in plain render mode) - picks a color palette
   before anything else paints.
2. **Provider wizard** - runs only when no provider is configured yet:
   - pick a provider from a grouped menu: Local servers (llama.cpp, LM
     Studio), Other (any OpenAI-compatible URL), then the hosted vendors
   - credentials: an already-set conventional env var (`ANTHROPIC_API_KEY`,
     `OPENAI_API_KEY`, ...) is used as-is and the secret stays off disk,
     otherwise a hosted provider asks for a pasted key (leaving it empty
     defers to the env var) and a local one asks for its server URL
   - models: the endpoint is probed for discovery when it supports it, then a
     default model is picked from the discovered list. Hosted providers with
     no reachable list accept one manually entered model id
   - the result is merged into `~/.ajent/models.json` (existing providers are
     never lost) and loaded into the running session

Quitting mid-setup abandons it cleanly, and nothing is written until the
provider step completes. The wizard can be revisited by hand at any time by editing
`models.json` directly (`models.md`). Serve mode skips the wizards with a
notice instead.

A headless first run (no model configured) does not interview anyone: it exits
1 with `no model configured; set one with -m or in config.json`.

## Choosing a front end

The command line picks what kind of process ajent becomes. Short-circuiting
actions exit before any session opens:

| Action | Behavior |
|---|---|
| `-v`, `--version` | print the version, exit 0. Works regardless of every other flag or a broken home dir |
| `--update` | reinstall from @latest, then exit (see above) |
| `--delete <id\|name>`, `--delete-old [days]` | remove saved sessions, then exit (see below) |

Everything else picks a front end:

| Invocation | Front end |
|---|---|
| terminal stdin and stdout | interactive TUI, positional arguments seeding the first prompt |
| `-p/--prompt <text>` | headless one-shot: one turn, print, exit |
| piped or redirected stdin (or stdout) with no `-p` | headless one-shot, prompt read from the pipe or from positional arguments |
| `--serve` | no terminal at all. The control socket is the only front end and prompts arrive over it |
| `--attach [name\|id\|socket]` | mirror a live session on this terminal, opening no local transcript |

Note that `ajent "some prompt"` typed at a real terminal opens the TUI with
that first prompt, it does not run headless. Scripts must use `-p` or a pipe.

## Command-line flags

### Global

| Flag | Default | Description |
|---|---|---|
| `-m`, `--model <key>` | config `model`, else the first known model | initial model for the session. A name that resolves to nothing warns and falls back |
| `--render <mode>` | `auto` | paint mode: `auto`, `inline` (terminal scrollback, unsupported under tmux/screen), `alt` (own scrollback), `plain`. Details in `tui.md` |
| `--system <text>` | unset | replaces ajent's default prose guidance (the opening sentence and guidelines) in the system prompt. Environment facts and project instructions still follow. Overrides `agent.systemPrompt` for this run |
| `--force` | off | with `--delete`: remove a session another instance holds live. With `--attach`: take the control seat from its holder. Refused beside anything else |

### Session selection

Precedence: `--session` beats `--resume`, and either beats `--continue`.
Matching rules (id, unique prefix, name) and naming are covered in
`sessions.md`.

| Flag | Default | Description |
|---|---|---|
| `--continue` | off | resume the most recent session automatically |
| `--session <name>` | unset | resume the named session, creating it when new. A fresh name makes this run start a transcript that later runs can find by name |
| `--resume [id\|name]` | off | with a target: reopen that exact saved transcript. Bare `--resume`: interactive picker of saved sessions |

### Permission scope

All default off, which leaves the configured mode alone (interactive:
`permissions.mode`, one-shot: `permissions.headlessMode`, else auto+write).
Mutually exclusive with each other. What each mode auto-runs is described in
`permissions.md`.

| Flag | Description |
|---|---|
| `--allow-all` | barrier starts at allow-all, everything including bash runs without approval |
| `--read-only` | only verifiably read-only tools are offered (built-in readers plus declared-read-only MCP tools), starting at auto. Not a prompt-only guarantee: the model still gets tools |
| `--allow-auto` | every built-in except core writers is offered, starting at auto: unverifiable calls are model-classified and a one-shot takes that verdict as final |
| `--allow-autowrite` | like `--allow-auto` plus workspace-confined writes run without approval |
| `--block-autodenied` | interactive runs deny what the model classifier does not approve instead of opening an approval dialog. Accepted on a one-shot for script parity and changes nothing there (one-shots never open dialogs to begin with) |

### One-shot only

These require `-p/--prompt`. Without it they are usage errors, so piped runs
cannot set them (see Scripting recipes for the workaround).

| Flag | Default | Description |
|---|---|---|
| `-o`, `--output <shape>` | `text` | output shape: `text` streams the final answer to stdout, `json` emits one event per line (below) |
| `--no-tools` | off | withhold the whole tool set, built-ins and MCP alike. Guarantees the model answers from its prompt alone, and no MCP server is even spawned. Overrides `--allow-tools`/`--deny-tools`, which then error as meaningless |
| `--allow-tools <names...>` | none | pre-granted session allows: a registered tool name (writers included) runs that tool, `bash` grants any nameable shell call, and any other single word is granted as a bash command head (`--allow-tools python3`). Grants hold across the mode changes that clear earned approvals. A multi-word entry warns and names nothing |
| `--deny-tools <names...>` | none | withhold named tools from the offered set entirely |
| `--stats` | off | print a tool and token summary when the run ends: prose block on stderr with `-o text`, one `summary` event line with `-o json` |

### Serve, attach, delete

| Flag | Description |
|---|---|
| `--serve` | run with no terminal, the control socket as the only front end (implies `"control": {"enabled": true}` whatever config says). Prompts and dialogs hold until a controller answers. Ends when a controller ends the session or on signal, exiting 0 with the resume hint on stderr |
| `--attach [name\|id\|socket]` | attach this terminal to a running session and mirror it live, interacting from either side. Bare `--attach` opens a picker of live sessions. Claims the control seat (`--force` takes it from its holder). Needs no models file and opens no transcript. Every flag configuring a local run (`-m`, `--system`, permission scope flags, all one-shot flags) is refused |
| `--delete <id\|name>` | delete that saved session, then exit. A live session refuses unless `--force` is given |
| `--delete-old [days]` | delete every *unnamed* session in the workspace unused for 28 days (or the given count), listing them and asking to confirm first. Live sessions are skipped, never forced. Mutually exclusive with `--delete` |

### Validation

Any violation prints one `ajent: ...` line on stderr and exits 1 before
anything runs. The enforced rules:

- the five permission scope flags above are mutually exclusive
- `--no-tools` cannot be combined with `--allow-tools` or `--deny-tools`
- `--session` cannot combine with `--resume` or `--continue`. Names must be
  non-empty, short, and free of flag-like characters
- `--force` only applies to `--delete` or `--attach`
- the delete actions accept nothing else: no session selection, prompt,
  serve/attach flags, or positional arguments
- `-p/--prompt` cannot combine with positional arguments (the flag takes the
  whole prompt), and refuses a blank value. Bare `--resume` alongside `-p`
  errors because the picker needs a terminal
- `--serve` takes no prompt of any kind
- one-shot-only flags require `-p`. An unknown `-o` shape is rejected here

## Headless one-shots

A headless run answers exactly one prompt with no terminal and no dialogs,
then exits. It never dispatches slash commands or shell lines, and how the
permission barrier settles an ask without a dialog is `permissions.md`'s.

### Prompt sources

The first source that yields a prompt wins:

1. `-p/--prompt <text>`, which outranks everything: piped bytes beside it are
   never read
2. a terminal pair (stdin and stdout both TTYs) opens the interactive TUI
   instead, positional arguments seeding its first prompt
3. positional arguments, joined with spaces, when either stream is not a TTY
4. piped stdin, trimmed (`echo "question" | ajent`)

Two misuses are usage errors (exit 1): typed input with redirected stdout has
nothing to run and nowhere interactive to go, and an emptied pipe leaves no
prompt. `@path` references in the prompt expand exactly as they would for a
typed line (`commands.md`), including image references on vision models.

### Recording

A one-shot records its transcript like any session (persistence failures only
warn), so scripts stay composable: `ajent --continue` or
`ajent --resume <id|name>` rejoins it later, interactively or in another
one-shot. Use `--session <name>` to make the target stable across runs:

```sh
ajent --session nightly -p "triage new issues"
ajent --resume nightly  -p "draft replies for the top three"
```

A bare `--resume` needs its interactive picker, so headless refuses it (exit
1) rather than silently starting fresh. The prompt also lands in the editor
history, recallable with ↑/↓ and Ctrl+R from a later interactive session.

### Exit codes

| Code | Meaning |
|---|---|
| 0 | the turn completed and produced a final answer. Also every successful short-circuit (`--version`, `--update`, deletes) |
| 1 | anything that failed before or around the turn: bad flag combinations, unreadable config or models file, no model configured, an unresolvable session target, refused bare `--resume`, delete failures, update failure |
| 2 | the turn itself did not produce a final answer: provider error after retries, interrupted before an answer, or the model stopped without one |

With `-o json` the same mapping rides the final `result` event as
`status`/`exit`: `ok`/0, and `error` or `empty`/2. Any non-ok exit also prints
a one-line reason on stderr (both shapes), so logs stay explainable while a
json stream stays pure on stdout.

### Signals

SIGINT, SIGTERM and SIGHUP cancel the run: the turn aborts, unanswered tool
calls get error results so nothing dangles, and the process exits 2 with
status `empty` ("interrupted before an answer").

### --stats

`--stats` reports only what this invocation did (a resumed session's prior
spend is baselined away):

- `-o text`: a `--- session summary ---` block on stderr with turns, wall
  time, per-tool call counts with failure counts, token totals
  (input/output/cache-read/cache-write), and one line per model used. stdout
  keeps the answer alone
- `-o json`: one `summary` event line before `result`

## JSON events (`-o json`)

One JSON object per line on stdout, and nothing else ever touches it. Fields are
omitted when unset, so each line carries only what its type means. Pre-turn
setup problems (bad config, no model) happen before any event exists: stderr
gets the message and exit is 1 with an empty stream.

| Event | When | Fields |
|---|---|---|
| `turn_start` | first, once per run | `model` (provider/model key) |
| `text` | each completed assistant prose block, whitespace-only blocks dropped | `text` |
| `tool_call` | when the model calls a tool | `id`, `name`, `input` (raw arguments) |
| `tool_result` | when that call lands, paired by `id`. Parallel dispatch may interleave pairs | `id`, `name`, `output`, `error` (true only on failure) |
| `notice` | mid-run notices (warnings, interrupts) | `level` (`info`/`warn`/`error`), `text` |
| `turn_end` | once, after the last step | `stop`, `steps`, `usage` |
| `summary` | only with `--stats`, between `turn_end` and `result` | `turns`, `seconds`, `calls`, `failed`, `usage`, `byModel` (per-model usage) |
| `result` | always the last line, ending the stream | `status` (`ok`/`error`/`empty`), `exit` (the process code, 0 or 2), `text` (final answer when non-empty) |

The `usage` object is
`{"input","output","cacheRead","cacheWrite","reasoning"}` in tokens.
`stop` uses the provider-neutral vocabulary: `end_turn`, `tool_use`,
`max_tokens`, `incomplete`, `aborted`, `error`.

A refusal surfaces as a `tool_result` with `"error":true` and output
`permission not given`. An interrupted run still ends with its `result` line.

## Scripting recipes

```sh
# simplest form: pipe the prompt, get prose on stdout
echo "explain what this repo does" | ajent

# read-only review of uncommitted work (tools offered are verifiably read-only)
git diff | ajent --read-only -p "review this diff for correctness issues"

# guaranteed tool-less answer: the model sees only the prompt
ajent --no-tools -p "what is a re-entrant lock?"

# capture just the final answer from an event stream
ajent -p "list flaky tests" -o json | jq -r 'select(.type=="result").text'

# watch tool activity as it happens
ajent -m anthropic/claude-opus-4 -p "audit auth" -o json \
  | jq -c 'select(.type=="tool_call")'

# branch on outcome: 0 answered, 1 setup problem, 2 failed turn
ajent --continue -p "fix the failing build" || echo "run failed"

# one-shot-only flags need -p, so wrap stdin when you also want them
ajent --read-only --stats -p "$(cat question.txt)" -o json > events.jsonl
```

Text mode streams prose to stdout as it arrives (tool progress and notices go
to stderr) and appends one hint line after the answer:
`Run \`ajent --resume <id|name>\` to resume this session.` Scripts that need a
clean answer should strip that line or prefer `-o json`, which never prints it.

## Deleting sessions from the shell

Both delete actions act on the current workspace only and exit without
starting a session (exit 0 on success, 1 on failure):

- `ajent --delete <id|name>` resolves its target like `--resume` does, then
  removes the transcript. A session another ajent instance holds live refuses,
  naming it. Add `--force` to remove it anyway.
- `ajent --delete-old [days]` lists every unnamed session unused past the
  window (28 days by default) and asks for confirmation on stdin before
  removing anything. Named sessions are never touched.

Rules for what gets deleted, live-session detection and concurrency are in
`sessions.md`.

## See also

- `permissions.md` - permission modes, the headless refusal contract,
  safe/denied matching
- `config.md` - every config.json key, its layer, and `AJENT_*` bindings
- `models.md` - models.json providers, discovery, model declarations
- `sessions.md` - naming, resume matching, rewind/fork, deletion rules
- `commands.md` - slash commands and the input-line grammar (`@path`, `!shell`)
- `tui.md` - keybindings, rendering, status line
