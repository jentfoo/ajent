# Commands

What one submitted line can be: prompts, slash commands, shell runs and
`@path` references, plus every slash command with its arguments and behavior.
Keybindings live in `tui.md`, what each permission mode auto-runs in
`permissions.md`, the surfaces behind `/plan`, `/agents` and `/mcp` in
`agents.md`. A headless one-shot never dispatches any of this (`cli.md`),
it answers a single prompt and exits.

## Input-line grammar

Every submitted line is classified once, at submit:

| You type | It becomes |
|---|---|
| `text` | a prompt for the model |
| `/name args` | a slash command |
| `!cmd` | a shell run whose output joins your context (below) |
| `!!cmd` | a shell run shown only to you, never reaching model context or transcript |

Three escapes force a literal prompt:

- a leading space: that one space is dropped and the rest sent verbatim
- `//text`: sent as the prompt `/text`
- a multi-line paste opening with `/name args`: sent as one verbatim prompt,
  so pasted content is never consumed by command dispatch. A multi-line `!`
  keeps its staged shell run, since staging was what you typed.

Command names are case-insensitive and argument case is preserved: `/MODEL
claude-...` reaches `model`. A bare `/` or an unknown name is a warning
notice naming the typo, never a turn. Whitespace-only lines do nothing.

### Prompt lines

A prompt expands its `@path` references (below), then becomes the session's
next input. While a turn streams your line queues as a dimmed row above the
prompt and hands over at the next step boundary or behind the next turn's
prompt, so steering mid-turn needs no interrupt. Queued lines stay
recoverable into the editor until they are delivered (`tui.md`).

### Slash command lines

Commands run immediately, even mid-turn. Handlers that need quiet refuse on
their own: `/compact` and `/init` ask you to stop the turn first while
everything else applies at once. A handler fault degrades to an error notice,
so a broken command never ends the session.

MCP servers connect in the background while you type; opening `/tools` or
`/mcp`, or sending your first prompt, waits for that to finish so tool schemas
exist before anything reads the tool set (`mcp.md`).

### Shell lines: `!` and `!!`

`!cmd` runs through the same bash path a model tool call uses, marked as
user-initiated, so it never asks for approval and still works after you
disable `bash` for the model in `/tools`. There is no forced timeout by
default. Output becomes one user-authored message, `User Ran: <cmd>` plus its
output, staged ahead of your next prompt:

- while staged it counts toward the context bar, so nothing sneaks in unseen
- when you submit the next prompt, delivery waits for staged runs to finish
  and flushes them first, then your message rides on top
- interrupting a run (Esc) stops it but still stages what it printed so far,
  marked as interrupted
- a rewind or fork discards everything staged against the abandoned branch

`!!cmd` is the same run for your eyes only: full display like any tool call,
but its output never reaches model context, the transcript or the staged
token count. It also never delays a prompt.

An empty command (or a session where bash never registered) is a warning
notice and nothing else.

## @path references

`@` in a prompt pulls real content into that message without a turn of tool
use:

- a small text file is injected in full, line-numbered, as synthetic read
  call/result pairs appended behind your text. They live on your branch, so a
  rewind onto the message drops them with it
- a directory or a glob (`@docs/*.md`) injects an `ls` listing of what is
  there, so you and the model agree on the names before anything is read
- an image reference on a vision model attaches the picture itself. On a
  text-only model it stays visible as an annotation instead

Anything not injected in full is annotated in place with its shape:
`@big.log (4,200 lines, 2.1mb)`, `(binary, 340kb)`, `(image, 1.2mb)` or
`(special)` for pipes and devices. The model sees the reference exists and
can decide to read it.

Rules worth knowing:

- `@` only opens a path at a word boundary, so `email@example.com` is prose.
  Trailing punctuation ends the token
- paths resolve from the workspace root like the read tool's, and `~`, `./`
  and absolute forms are kept as written
- each path injects once per message however often it is named, and content
  already in context unchanged since its last injection is never duplicated.
  Re-expanding an annotated reference replaces its measurement instead of
  stacking a second one
- defaults: one reference up to 500 lines / 128kb, 128kb total per message.
  Above either bound the reference annotates. `tools.limits.refInject` and
  `tools.limits.refTotal` re-tune both (`config.md`)
- references run through the read/ls tools without approval regardless of
  what `/tools` has enabled. They are your instruction, not the model's

## Completion

- `/` at line start opens a live menu of commands with their descriptions as
  you type. After the name, Tab completes that command's argument: model keys
  and aliases for `/model`, levels for `/reasoning`, section names for
  `/settings`, verbs then server names for `/mcp`, `list`/`stop` then job ids
  for `/agents`
- `@` completes paths one directory at a time, each level listing on its own.
  Candidates rank by already in the conversation, then recent modification,
  then fuzzy match. Directories complete with a trailing `/` to keep drilling
- `!` and `!!` lines complete like bash: command names while you are naming a
  command (also after `|`, `&`, `;`, `(`, `{`), paths anywhere else

## The commands

In menu order. Commands with deep behavior elsewhere point at it in place
of repeating it.

| Command | Args | Behavior |
|---|---|---|
| `/help` | `[question]` | bare: markdown list of every registered command and the keybindings, closing with where questions go. With a question: answer from ajent's built-in documentation in one shot, printing nothing else itself. Nothing reaches the transcript or tool set |
| `/model` | `[name]` | with a name, switch to that model (key or alias, ambiguous names warn with the candidates). Bare, open the picker preselected on the current model. The switch applies at once, is recorded in the session, and is saved to your user config so new sessions keep it |
| `/reasoning` | `[level]` | set the reasoning level for the active model. Bare shows a picker of what this model supports (or reports when there is no choice). A named level the model cannot send clamps to the nearest supported one with a notice. Saved as a session override that resumes (`models.md`) |
| `/usage` | none | print the token ledger: context fill against the compaction threshold, the response reserve, turns with input/output totals, sub-agent spend when there is any, and a per-model table when more than one model ran |
| `/copy` | none | copy the last agent response to the clipboard as text plus every tool call and result verbatim. During streaming it copies the last settled response, never a partial turn |
| `/compact` | `[instructions]` | shrink context toward the compaction threshold now, with the argument as the summariser's focus. Refused while a turn streams (`sessions.md`) |
| `/tools` | none | open the tool picker for this session (rules below) |
| `/session` | `[name]` | bare reports the current name or says the session is unnamed. With a name, name it so `ajent --resume <name>` finds it later. A malformed or already-taken name is a warning notice (`sessions.md`) |
| `/mcp` | `[connect\|disconnect\|logs\|reload] [name]` | bare prints the server table (state, transport, tool count, latency). The verbs connect, disconnect, show logs and re-read `mcp.json` live (`mcp.md`) |
| `/reload` | none | re-read `config.json`, `models.json`, `mcp.json` and `AGENTS.md` into the running session. Tools a reload discovers arrive disabled until `/tools` enables them |
| `/agents` | `[list\|stop <id>\|all]` | bare lists every sub-agent job (id, status, elapsed, task). `stop` cancels one id or all in-flight ones (`agents.md`) |
| `/settings` | `[section]` | open the settings menu. A section name jumps straight to that editor and returns, while bare stays in the menu until dismissed |
| `/update` | none | check for a newer release and reinstall it, reporting by notice while you keep typing (`cli.md`) |
| `/exit` | none | quit the session |
| `/plan` | `[goal]` | start the two-model plan workflow (`agents.md`) |
| `/plan-stop` | none | stop the workflow, restoring the original model and tools (`agents.md`) |
| `/plan-status` | none | report the phase, round and both models on one line (`agents.md`) |
| `/init` | none | survey the project and write `AGENTS.md` (below) |

### /tools before and after the first prompt

The picker lists tools grouped by source (built-ins, each MCP server, the
sub-agent group when enabled). Before this run's first dispatched prompt the
selection is free: enable or disable anything, with a resumed session
preselecting its recorded set. After that it is widen-only, offering just the
disabled tools, because a tool block the model has already seen cannot be
retracted. Any change prints `tool block changed; prompt cache will miss`, so
settle the picker before a long working block when you care about cost.

### /settings

Each row edits one setting live as a session override, then a save prompt
offers to keep it for this session only or copy it into your user or project
config file. Rows: Model, Reasoning, Reasoning retention,
Show thinking, Tools, Auto-compaction, Compaction verbatim steps, Compaction
verbatim size, Permissions mode, Sub-agent model, Sub-agent concurrency,
Sub-agent step limit, Block images, Theme, Tool limits and Reset overrides.
The keys behind them, their layers and `AJENT_*` bindings are in `config.md`.

### /init

Runs a read-only project survey in the background (it may delegate to
sub-agents), then submits the result as an ordinary turn that writes
`AGENTS.md`. One survey at a time, refused while a turn streams ("press Esc
first"), and cancellable.

### Project instructions

`AGENTS.md` in the working directory (and `~/.ajent/AGENTS.md` for habits you
want in every workspace) is read once at startup and injected into every turn's
system prompt. Edits apply on the next start, or immediately with `/reload`,
which is why a fresh `/init` points you there after it changes the file.

## See also

- `tui.md` - keybindings for every surface, the completion keys, queued rows
- `permissions.md` - what auto-runs per mode, approval dialogs
- `agents.md` - sub-agents, `/plan`, MCP servers and their tools
- `sessions.md` - compaction behind `/compact`, session names behind `/session`
- `config.md` - the keys `/settings` writes and the reference limits
