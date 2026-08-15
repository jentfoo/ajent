# Command design

How `pkg/command` and `pkg/refs` own everything the user does that is not "send
a message to the model": slash commands through a registry, direct `!` shell
execution with staged results, `@`-path expansion with auto-read, and the
non-blocking completion overlay driving both. It is the single dispatch path for
submitted lines; extensions and MCP register into the
same registry, so a built-in `/tools` and a third-party `/plan` are the same
mechanism.

## What it is

Three packages sit above the agent, tools and TUI:

- `pkg/command` — `Command`, the registry, the `Console` interface, the
  built-in commands, the shell `Stager`, and the aggregating `Completer`.
- `pkg/refs` — `@` parsing, the inject-or-annotate expander, and the
  gitignore-aware path index for completion.
- the completion overlay in `pkg/tui` — the live block state distinct from an
  interaction, where the editor keeps focus.

`pkg/command` imports `pkg/tui`, `pkg/agent`, `pkg/llm`, `pkg/tools` and
`pkg/session`; it never imports `pkg/refs` (the host wires the two together
through the `Completer`). The UI does not import the command package — the host
adapts `Console` onto `*tui.UI`, so headless mode and the sub-agent
can drive the same commands with a different front end.

## Dispatch

A submitted line is classified once, then routed:

```go
type Kind uint8
const ( KindPrompt Kind = iota; KindCommand; KindShell )

func ParseLine(s string) Line
```

`ParseLine` covers every escape hatch in the spec:

| Input | Kind | Rest |
|---|---|---|
| `hello world` | prompt | `hello world` |
| `/model gpt` | command | `model gpt` |
| `//literal` | prompt | `/literal` |
| ` /notacommand` | prompt | `/notacommand` (leading space escapes) |
| `!go test ./...` | shell | `go test ./...` |
| ` !echo hi` | prompt | `!echo hi` (leading space escapes) |
| `!!echo hi` | prompt | `!echo hi` (double-bang escapes) |

Unknown `/foo` still parses as a command so dispatch can notice it rather than
prompting the model — a typo should not cost tokens. The host's loop then routes
by kind: shell lines go straight to the `Stager` (non-blocking); command and
prompt lines go to a single **prompt pump** goroutine that owns ordering.

### The prompt pump

`main.go`'s loop is a thin classifier feeding one pump channel:

- loop: `ParseLine` → shell to `Stager.Run` (non-blocking); command and prompt
  lines to the pump; the `quit` case is unchanged.
- pump: a command runs its handler (pickers block only the pump); a prompt
  `Stager.Flush` → `refs.Expand` → `agent.Input{Text, Before}` → steer or start.

Submissions therefore stay in order, the UI never stalls, and "the turn is held
until the pending command finishes" falls out of `Flush` blocking the pump. Only
prompts flush — `!ls` followed by `/model` leaves the stage pending for the next
real message. The CLI seed (`ajent "explain @main.go"`) goes through the pump
too, so its references expand like any other prompt.

## Commands

```go
type Command struct {
    Name        string
    Description string
    Args        string // usage hint, e.g. "<optional-instructions>"
    Complete    func(prefix string) []string // argument completion, optional
    Handler     func(ctx context.Context, args string, c Console) error
}
```

`Complete` is for *argument* completion: past the first space the matched
command supplies its candidates. `/model` supplies model keys and aliases;
`/reasoning` supplies `llm.Levels()`; a command with a nil `Complete` offers
nothing.

### Registry

```go
type Registry struct{ ... }
func (r *Registry) Register(cmd Command)        // add or replace; last wins, order kept
func (r *Registry) Get(name string) (Command, bool)
func (r *Registry) List() []Command             // registration order, for /help
func (r *Registry) Names() []string
```

`Register` is add-or-replace so it can be widened to cover more behaviour; the
registration order is preserved for `/help` and command completion.

### Console

`Console` is a command's view of the world — an interface, not a struct, so
the extension host can back it with its own protocol:

```go
type Console interface {
    Notify(msg string, level tui.Level)
    Print(markdown string)                       // /help renders a markdown list
    Pick(ctx, prompt, items, opts) (int, error)
    MultiPick(ctx, prompt, items, opts) ([]int, error)
    Select(ctx, prompt, options) (int, error)    // enum picker for /settings rows
    Confirm(ctx, prompt) (bool, error)           // yes/no for toggles
    Input(ctx, label, placeholder) (string, error) // free text for numbers/limits

    Models() *llm.Registry
    State() *agent.State
    Tools() *tools.Registry
    Commands() *Registry
    Settings() *config.Set                       // resolved configuration handle
    SaveSetting(layer, key string, value any) error // write to user/project layer
    SetSessionSetting(key string, value any) error  // session override + recording

    SetModel(m llm.Model)                        // registry + state + status + session entry
    SetReasoning(c llm.ReasoningConfig)
    ToolsChanged()                              // persists the enabled set to the session
    Started() bool                              // has a user prompt been sent this session
    Exit()
}
```

`main.go` implements it once (`uiConsole`) over the objects the driver already
holds. `SetModel` records a `model_change` entry the old bespoke switch never
did; `ToolsChanged` records a `setting_change("tools.enabled", names)` (dotted
config key) so resume keeps the set. The three interaction methods are one-line
forwarders to `tui.SelectContext`/`ConfirmContext`/`InputContext`, and the two
settings methods delegate to the resolved `*config.Set`. Each mutator also calls
`SetSession` with its config key so `/settings` reports the change as `(session)`.
`SetSessionSetting` applies a dotted session override *and* records it via
`recorder.SettingChange`, closing the old gap where a row called `SetSession`
directly and the override silently vanished on resume (auto-compaction, tool
limits). The bespoke `SetModel`/`SetReasoning` mutators keep their own paths.

`Started()` is owned by the pump: it flips true when the first prompt is
dispatched, never on a command or a `!`.

### Built-ins

| Command | Behaviour |
|---|---|
| `/help` | markdown list of commands and keybindings through `Console.Print` |
| `/model [name]` | resolve by name, or open the picker; records a model-change entry |
| `/reasoning [level]` | report, or set/clear the level for capable models |
| `/tools` | multi-select, grouped by source; widens the enabled set |
| `/settings [section]` | two-level menu of rows showing value + source layer; each row edits and offers save-to-layer (`see config-design.md`); generic `enumRow` (string key from a fixed set) and `modelRow` (key through the model picker) builders cover phase 12's mode and phase 13's sub-agent model |
| `/exit` | quit |

`/settings`, `/compact`, `/resume`, `/cost` and `/init` also register into
the same registry.

### `/tools` and the enabled set

The set only ever *widens* within a session, because the tool block the model
has already seen is not retractable — history may hold calls to a tool, and
dropping it would leave the transcript describing a tool that no longer exists:

- **Before the first prompt** the picker lists every registered tool
  (`Registry.All`) and the selection is free: enable or disable anything.
- **After the first prompt** the picker lists only `Registry.Disabled()`;
  selecting enables them via `Enable` (additive, vs `SetEnabled`'s replacement).
  Nothing can be turned off again for the rest of the session.

Widening the set changes the tool block and therefore busts the prompt cache
(worth a one-line notice). `/tools` groups rows by `Registry.Source` (builtin,
an MCP server name, an extension name), with a dim header row when the group
changes. Space or Tab toggles the highlighted row; typed text narrows the filter
(a space never becomes part of it) and Enter confirms.

## Shell mode (`!`)

`command.Stager` owns staged runs:

```go
func NewStager(reg *tools.Registry, sink agent.Sink) *Stager
func (s *Stager) Run(cmd string)        // starts immediately, non-blocking
func (s *Stager) Pending() bool
func (s *Stager) Cancel()
func (s *Stager) Flush(ctx) []llm.Message // waits, returns call+result pairs
```

`Run` resolves `bash` through `Registry.Get` (enabled-only, so a disabled `bash`
is a refusal notice), mints a call id, opens the display with
`sink.ToolStart` and executes on a goroutine through `agent.NewOutput`. That
reuses the whole `bash` path — streaming, ANSI stripping, output cap, spill
file, process-group kill — with no second implementation.

**Staging onto context.** The command text and its result are *not* sent to the
model immediately. They sit in the pending slot ahead of the next user message.
Running `!` for exploration costs no tokens and never interrupts an in-flight
turn — it only becomes visible when the user next speaks.

**Flush timing.** On the next normal submit the pump first `Flush`es the stager
— which waits for every staged command to finish, in submission order — then
injects each pair ahead of the user message. The model therefore sees "here is a
shell invocation I asked you about and its output" together with whatever the
user just said. Multiple staged commands flush together, in submission order,
each as its own call + result pair.

**Representation.** Reuse the real `bash` tool end to end. The staged pair is a
synthetic shell-call + result block carrying provenance (`shellProvenance{Source:
"shell", TS}` in `ToolResultBlock.Details`) so token accounting can count it
and compaction's superseded-pass can treat it as injected content.

**Error behaviour.** A non-zero exit code is an ordinary staged result, not a
turn failure — the model sees the failure (with the exit code) at the next
message. `!` alone or an empty command produces a notice and runs nothing. A
literal line beginning with `!` leads with whitespace (` !echo hi`).

**Cancellation.** Each staged run owns a cancellable context. `Cancel` cancels
every still-running run; the `bash` tool kills the whole process group when its
derived context ends. A cancelled command still stages: its partial output plus
the killed-status the bash tool records, so the model's view matches what the
user saw. `Ctrl+C`/Esc cancels an in-flight staged command through the host's
interrupt path.

> Staged `!` results live only in memory until the next prompt flushes them. If
> the process exits before that flush, the staged results are lost — they are
> not part of the transcript until the model sees them.

## Completion overlay

Two triggers, one mechanism:

- `/` at the start of an empty line → command completion.
- `@` anywhere → path completion.

The overlay is a distinct live-block state from `u.act` (an interaction): the
editor keeps focus and keeps receiving characters. Only `Tab`/`↑`/`↓`/`Enter`/
`Esc` are consumed before the editor; everything else falls through so typing
still narrows the list. After every editor mutation the completer is re-queried.

```go
type Completion struct { Text, Label, Detail string; Score int }
type Completer interface {
    // Complete is called under the UI lock from the key goroutine; it must not
    // block or call back into the UI. start is the grapheme index the accepted
    // Text replaces, up to the cursor.
    Complete(text string, pos int) (start int, items []Completion)
}
func (u *UI) SetCompleter(c Completer)
```

The path source keeps a cached index for exactly this reason — `Complete` may
not block.

### Accept rules

- `↑`/`↓` cycle the highlight and mark the selection *moved*.
- `Tab` accepts the highlighted candidate into the editor immediately; a
  following `Enter` just submits the result (e.g. `/model` then Enter runs it).
- `Enter` with a moved selection (`↑`/`↓`) accepts that candidate and submits;
  with the list merely offered it submits the line as typed, so an open list
  never swallows a send the user meant.
- typing after a selection drops the *moved* state again — Enter then sends what
  was typed rather than re-applying a stale highlight.
- `Esc` dismisses the overlay without inserting anything.

### `command.Completer`

The aggregating completer sources commands from the registry and paths from
`refs.Index`. `/` at line start offers command names; past the first space it
delegates to the matched command's `Complete`. `@` anywhere offers workspace
paths. A nil path index disables path completion (plain mode).

## `@` file references

### Parse (`refs/parse.go`)

`Parse(text)` returns `Ref{Path, Start, End, Note}`. `Note` carries any existing
`(800 lines, 64kb)` measurement absorbed into the token — that absorption is
what makes re-expansion idempotent.

`@` matches only at a word boundary (start of line, or after whitespace, `(`,
`[`, a backtick or a quote) so `email@example.com` is prose. The path token
stops at whitespace or trailing punctuation. The annotation shape is matched
strictly: a trailing `(...)` is absorbed only when it contains a digit or a
known kind word (`binary`, `image`, `dir`, `lines`) and is made of the allowed
tokens (digits, `b`/`kb`/`mb`, the kind words), so ordinary parenthetical prose
after a path is never eaten.

### Expand (`refs/expand.go`)

`Expander.Expand(ctx, text)` returns the rewritten text, the `[]llm.Message`
for `Input.Before`, and notices. Per distinct resolved path (via the tool
`PathPolicy`):

| Case | Outcome |
|---|---|
| missing | literal, warning notice |
| directory | `ls` pair injected via `Registry.Lookup` (ignores enabled state) |
| already read, unchanged (`Tracker.Check == nil`) | nothing injected, literal |
| text file within `RefInject` and under the running `RefTotal` cap | `read` pair injected, stale annotation stripped |
| large, binary, image, or over the cap | annotation replaced in place; cap trim adds a notice |

Text is rebuilt by splicing spans back to front so earlier offsets stay valid.
Injected tools run through the same sink as the stager, so the display order
matches the transcript order and therefore matches what replay renders.

### Idempotence

Annotation is idempotent. The annotated text is what lands in history, so a
rewind that refills the editor, or any other resubmission of an earlier
message, sends text that already carries `(800 lines, 64kb)`. `Parse` absorbs
the existing annotation into `Note`, and `Expand` *replaces* it with the fresh
measurement rather than appending a second one.

### Index (`refs/index.go`)

`Index` enumerates files and directories once and refreshes on a TTL (5 s),
since `Complete` cannot block. Enumeration reuses the tool `find`'s approach:
`git ls-files -co --exclude-standard` in a repo for true `.gitignore`
semantics (directory entries derived from each listed file's ancestors),
`filepath.WalkDir` with the existing skip list otherwise. Ranking is
(a) already in the conversation (`Tracker.Records()`),
(b) recent mtime,
(c) fuzzy score (reusing `tui.MatchScore` rather than a second implementation).
Directories complete with a trailing `/` and keep completing.

### Measure (`tools/measure.go`)

`Measure(path)` stats first and, when the file is a small enough text file,
counts its lines. A directory reports `Dir` with zero bytes. A file above
`MeasureCeiling` reports its bytes and kind but never reads the whole content,
so annotating a giant file is itself bounded. `HumanSize` abbreviates the way
annotations show it: `64kb`, `1.2mb`.

## Pasted content

Large pastes (> ~2 kB) are stored on the UI and the editor receives a
placeholder (`[pasted 412 lines]`). The placeholder is what `editor.Submit`
records in history (that is the point), and `applyKey` expands placeholders
back to their content just before sending. The map is kept for the session so
recalling a pasted line from history still expands.

## Editor history

`pkg/tui` gained `Options.History` to seed the editor and `UI.History()` to
read it back. `pkg/history` persists that history to `~/.ajent/history` through
`config.UserPath`, deduplicated, capped at `MaxLines` (1000), and with the
secret prefix excluded on *both* load and save so a pasted secret never reaches
disk. The host owns the secret prefix; `history.Load`/`history.Save` take it as a
parameter.

## File map

`pkg/command`:

```
parse.go        ParseLine, SplitCommand — line classification
command.go      Command, Registry, Register/Get/List/Names
console.go      Console interface (+ Select/Confirm/Input/Settings/SaveSetting)
builtin.go      /help, /model, /reasoning, /tools, /settings, /exit + RegisterBuiltins
model.go        /model, /reasoning (moved from main.go)
tools.go        /tools — free-select before, widen-only after first prompt
settings.go     /settings menu and per-row editors
shell.go        Stager — staged ! execution and flush
complete.go     Completer — command + path completion source
```

`pkg/refs`:

```
parse.go        Parse — @ references with annotation absorption
expand.go       Expander — inject-or-annotate
index.go        Index — gitignore-aware path index for completion
```

`pkg/tui` additions:

```
complete.go        Completion, Completer, MatchScore, SetCompleter
complete_impl.go   completionOverlay — the live block state and accept rules
prompt.go          MultiPick, multiPickState (grouped, Tab-toggle multi-select)
input.go           keyTab / keyBackTab decoding
ui.go              paste placeholders, Options.History, UI.History
```

`pkg/history`:

```
history.go        Load, Save — editor line history persistence
```
