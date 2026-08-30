# TUI Design

How `pkg/tui` works, why it is shaped this way, and the rules you must not break
when changing it.

## What it is

A terminal front end for a coding agent: a scrolling transcript of agent output
with a live input field and status line beneath it. No external TUI framework.
Dependencies are `x/term` (raw mode, size), `rivo/uniseg` (grapheme clusters and
widths), `goldmark` (markdown parsing, with GFM tables hand-laid by
`layoutTable`), `go-udiff`
(diffs) and `chroma` (syntax highlighting).

Goals, in priority order:

1. The whole session stays in scrollback and survives a terminal resize.
2. Minimal chrome. Output gets the screen; the prompt area (divider + input +
   status) costs 3 rows.
3. Correct formatting: markdown, diffs, thinking vs reply, wide characters.

Goal 1 is the one that drove every hard decision below.

## Requirements

The originating brief, and what satisfies each item.

| Requirement | Where |
|---|---|
| Thinking output, shaded so it clearly reads as thinking; the pending line streams live like reply text | `UI.Thinking`, `Theme.Thinking` (dim + italic) |
| Markdown output | `UI.Text` -> `renderMarkdown` |
| Status line under the text field, reporting context usage | `status.go`, composed by `UI.repaint` |
| Text field accepting further user replies | `editor.go` + `input.go`, submitted over `UI.Messages()` |
| Edit actions show the whole file and highlight the changes | `UI.Diff` -> `RenderDiff`, shaded per line and intraline |
| Thinking visually distinct from reply output | semantic styles in `style.go` |
| As minimal as possible, maximum room for output | 2 reserved rows, no borders or rules |
| Let the CLI wrap the text where possible | text lines are emitted unwrapped in inline mode |

The last one is a preference rather than an absolute; inline honours it for
every text line. See "Wrapping policy" for the exceptions and why.

## Layout and chrome

Deliberately bare. No box, no borders anywhere except inside tables, with one
deliberate exception: a single narrow `─` divider rule sits atop the prompt area,
separating it from committed output above (see "Prompt divider" below).

```
<committed transcript>

❯ user input, continuation rows indent by two
  second line of a multi line message
▓▓▓░░░░░░░ ~68.2k/200k · opus-5
```

- Prompt is `❯ ` on the first row, two spaces on continuations.
- An empty buffer shows a dim `type a message` hint.
- The status block sits on one dim row by default (see Status line).
- A running tool adds one transient spinner row directly above the input. It is
  never committed to history; only its header and result are.
- Activity rows (`SetActivity`) put keyed, single-line status for in-flight work
  between any overlays and the input. Two producers: live sub-agent jobs (one row
  per running investigation, showing its task or most recent output) and
  **tool-call progress** — a call the model is still composing, keyed `call:<id>`,
  showing its name, target and the argument lines/bytes accumulated so far. Sizes
  go through `strutil.HumanSize` (binary, explicit `b`/`kb`/`mb`), never
  `strutil.FormatTokens` (decimal, unsuffixed), so a stream size is never
  misread as a token count. The
  latter fills the silence while a large `write` streams; it clears when the call
  completes and its `⏺` header and diff take over. Rendered in insertion order,
  each elided to width —
  never wrapped, so a row always occupies exactly one terminal line. Each status
  row is padded inside its background shade to fill the full terminal width
  (`shadeRow`), so live work reads as an edge-to-edge band rather than text with a
  colour patch underneath; the dim `+N more` overflow indicator stays unshaded.
  The true cap is `maxActivityRows = 3` text rows **plus** a dim `+N more`
  indicator (see
  `activity.go`). Activity is live-block only: it yields first on a short terminal
  and never reaches committed history.
- The input block grows with the buffer, capped at a third of the screen
  (`maxInputRatio`), after which it scrolls internally around the caret.

### Prompt divider

The prompt area is set apart from output by one dim full-width `─` rule rendered
directly above the interactive zone. It lives in the live block (`UI.repaint`)
rather than history, so row accounting stays exact (invariant 2) and it rides with
the input under every renderer — alt keeps a persistent frame, inline redraws it
as part of the moving block.

The rule is composed after streamed output but before overlays/activity/interaction,
so in-progress replies read as output above the bar while search/completion/
activity and the editor sit below. It costs one real row per repaint, which on a
short terminal shrinks interaction/dialog height by one line; tests that pin
dialogs to small screens are sized accordingly. The rule doubles as the only
keypress acknowledgement: a path Tab that can complete nothing accents it briefly
(`flashRule`) instead of changing the buffer.

Unlike activity rows the rule keeps the **full** composed width (one column
short of the terminal, like everything `repaint` composes) with no extra slack
column: it is a fixed glyph we chose, not caller text, so there is nothing to
sanitize and nothing to absorb — an ambiguous-width terminal renders the whole
rule at double width, which no one-column reserve could survive, and a visible
gap in a rule reads as a defect.

## Status line

The status block (`status.go`) is the fixed chrome beneath the input: a ten cell
context bar, used/total tokens, the model, then keyed `Segment`s in insertion
order. A running tool shows only its short name next to the spinner; the full
tool label (e.g. the bash command) lives on the committed header alone. The bar fills against the compaction budget (`window - reserve`) so a full
bar means "compaction fires now" rather than at raw capacity; the count shows
used against the real window. A `~` prefixes the count while it is an estimate
(mid-stream or between provider reports). The colour escalates at 70% and again
at 90%, both relative to the budget.

The model carries a short form (`ModelShort`, from `Model.ShortName`) and each
segment a `Short` form (fallback: full text) plus a `Priority`; a narrow
terminal shortens them in that order before anything splits. Packing
(`Status.rows`) is:

1. Everything on one row at full text, as long as it fits.
2. Otherwise the model shortens to its short form — first and always; it never
   vanishes.
3. Then segments shorten on the same row, in drop order (lowest `Priority`
   first, ties the later insertion).
4. Only when even all-short segments overflow does the block split in two. Row
   one is the fixed part (spinner, tool, bar/tokens) plus the model, shortening
   then clipping it. Row two packs the segments full-then-short, dropping in
   drop order only once every survivor is already short; survivors re-expand
   into freed width.

The block is capped at two rows; an overflowing segment line is clipped to width
rather than wrapped, so row accounting stays exact.

`SetStatusSegment(seg Segment)` is the single setter: add by a new key, replace
by key, remove with an empty `Text`. Because the live block is recomposed on
every repaint, a second row appearing and disappearing costs nothing structurally.
The front end publishes a `permissions` segment (`Key: "permissions"`) whenever
the live mode differs from the `allow-read` default — mirroring the reasoning
indicator. The non-default modes must always be visible so nobody forgets the gate
is open; it carries a short form (e.g. `all`, `block`, `auto+w`) for narrow rows.

The sub-agent manager publishes a `subagents` segment (`Key: "subagents"`) on
every transition — full form `subagents: 2 running (oldest 41s), 1 done`, short
form `sub 2` when only the count matters, cleared with an empty text when no jobs
exist. It carries a default priority and drops before `permissions` under narrow
widths, since permissions is a safety indicator that must stay visible.

The plan workflow publishes a `plan` segment (`Key: "plan"`) on every phase
transition — full form `plan: reviewing (r2/4)`, short form `plan r2` — cleared
when the workflow ends. It also drops before `permissions`. The workflow never
resets the screen: a phase switch changes what the *model* sees, not what the
user sees, so the whole run reads top to bottom with a divider per phase and the
segment is what makes the divergence inspectable.

## Layers

```
ui.go            public API, state machine, key handling, locking
  renderer.go        mode selection, terminal ownership, renderer interface
    render_inline.go   main screen, terminal owns wrapping, reflow and scrollback
    render_alt.go      alternate screen, we own wrapping and scrollback
  markdown.go      goldmark AST -> ANSI (+ hand-laid GFM tables)
  diff.go          go-udiff -> full file diff, shaded changes, intraline emphasis
  wrap.go          width aware wrapping, hanging indents
  editor.go        multi line input buffer, grapheme aware, plus its layout
  input.go         byte stream -> key events (escape sequences, paste)
  decision.go      approval dialog: context elision, numbered options, handle
  question.go      agent-initiated Ask: free-text or offered-option answer row
  activity.go      transient keyed rows above the input, and queued pending-prompt
                   rows (driver-owned), each capped with +N more
  status.go        status block model, two-row packing, keyed segments
  style.go         color profile detection, the palettes, the role table
  highlight.go     chroma -> per-line SGR for fenced code, keyed by the palette
  detect.go        terminal background classification (COLORFGBG, OSC 11)
  text.go          ANSI aware width, truncation, escape splitting
  ansi.go          escape sequence constants and builders
  history.go       line buffering for streaming input
  interaction.go   interaction queue, key routing, result channels
  prompt.go        Select, Confirm, Input, Pick and their live block rendering
  filter.go        subsequence matching and scoring for Pick
  notify.go        notices, level styles, safe collapse
  plain.go         interaction fallback for the mode with no live block
  errors.go        ErrCancelled, ErrBusy, ErrNoUI
```

Everything above the renderer layer is shared by all modes. If you are adding a
new kind of output, you almost certainly work in `ui.go` plus one renderer-
agnostic file, and touch no renderer at all.

The demo driving this is not part of this package: `ajent-demo` (the root module
built with the `demo` tag) spawns a standalone OpenAI-compatible model server in
`demo/`, so it renders real turns, tool calls and diffs flowing through the whole
agent loop rather than canned text.

## Render modes

The central design tension: native scrollback is owned by the terminal, which
reflows it on resize using its own rules. Anything we pin to the bottom lives in
that same buffer and gets reflowed too. You cannot both re-wrap history yourself
and keep the terminal's scrollback coherent.

A cell or frame-buffer renderer (bubbletea v2's ultraviolet) was evaluated on
the abandoned `refactor/bubbletea-tui` branch and rejected: it models cells,
not reflow, so when the emulator re-wraps a row into two every cell is still
"correct" while the buffer's origin on screen has moved — nothing in the cell
model can detect that. The relative erase from a cursor parked by the terminal
itself wins because only the terminal tracks that cursor through a reflow, so
this renderer keeps its one-terminal-row invariant enforced at the boundary.

So there are two modes, chosen by `ResolveMode`:

| | Inline | Alt |
|---|---|---|
| Screen | main | alternate (`?1049h`) |
| Who wraps prose | terminal | us |
| Who scrolls | terminal | us |
| Scrollback | native, whole session | ours, replayed on exit |
| Reflow on resize | emulator reflows every text line (prose and code alike); only tables and rules keep their committed width (see Known limits); the live block is redrawn at the new size | always correct (whole session, re-laid from retained lines) |
| Selection / scrollbar | native | on-screen only |

```
ModeInline                          ModeAlt
+--------------------------+        +--------------------------+
| committed output         |        | viewport over our buffer |
| more output              |        | (bottom aligned)         |
|                          |        |                          |
| > input          <- block|        | > input                  |
| bar/used tokens   follows|        | bar/used tokens    pinned|
|                          |        +--------------------------+
| (blank until output      |
|  fills the screen)       |
+--------------------------+
```

Selection rules:

- `$TMUX`, `$STY`, or `TERM` starting `screen`/`tmux` selects **alt**.
  Multiplexers do not reflow their buffers, so inline would degrade there.
- Not a TTY, or `TERM` empty or `dumb`, selects **plain** (no escapes at all).
- Everything else selects **inline**.
- `--render=auto|inline|alt|plain` overrides.

We can detect multiplexers with certainty. We cannot detect "does this emulator
reflow" in general, which is why the flag exists.

Alt paints absolute rows, so its geometry has two rules inline gets for free from
the relative park. History is bottom aligned into `viewHeight` = height − live
rows, which is **zero** when the block fills the screen (`repaint`'s final clamp
produces exactly that): a live row addressed past the last screen row is clamped
onto it by the terminal and silently overwrites its neighbour, so the block takes
every row and history simply yields. And `size()` re-reads the terminal like
inline does, so a frame composed mid-burst lays out at the width it paints at.
`clearHistory` repaints immediately rather than leaving the dropped rows on
screen until the next commit.

## Invariants

These are load bearing. Each one exists because breaking it produced a real bug.

**1. Inline never addresses a row it did not write.** No scroll region and no
`cursorTo` anywhere: every cursor move in `render_inline.go` is relative
(`cursorUp`, `\r`, `eraseLine`, `eraseBelow`) from the parked cursor, which the
terminal tracks for us through reflow and scroll. This is what makes the terminal's
own reflow and scrollback work exactly as they do for `cat`. In particular inline
mode does **not** re-render committed history on resize: after an emulator reflow we
cannot know how many physical rows our content occupies (widening pulls scrolled-off
rows back onto screen while tables and rules stay put; narrowing re-wraps
every text line), so rewriting it — absolutely or by a relative climb — lands on
rows that are not where we expect. Three designs tried to repaint the visible
screen from retained history and each surfaced new corruption on real terminals;
the live block, whose top is the parked cursor, is always redrawn at the current
width instead.

**2. Row accounting is exact, never predicted.** We emit N rows and the cursor
advances N rows. We do not compute "how many rows will the terminal wrap this
into" for anything already on screen. Predicting requires agreeing with the
terminal about width and about reflow, and any disagreement (a resize mid-write,
an emoji the emulator measures differently, an emulator that reflows unlike
ours) desyncs permanently, and the damage accumulates one row at a time because
by then it is committed content. This is why the erase carries no arithmetic at
all and the cursor is parked where the next erase must begin.

**3. Committed output is re-rendered only in alt mode; inline never rewrites a
committed line after it lands.** Streaming markdown commits at block boundaries
only (`splitCompleteBlocks`); an open block stays buffered until it closes. A
fence closes only on a line that is the opener's character repeated at least as
many times and nothing else (`fenceCloses`): an info-string line such as
```` ```go ```` inside an open fence is content, and treating it as a closer let the
rest of the block be scanned as top-level markdown — blank lines became commit
boundaries and the real closer reopened a phantom fence.
Re-rendering committed *scrollback* is what destroys history in tmux and VS Code — so inline never rewrites rows outside the viewport,
even on resize; every committed row keeps whatever layout it was given (and, for
prose, whatever the emulator's own reflow makes of it), which is exactly how `cat`
output behaves. The live block is redrawn at the current width on every repaint and
on a settled size change (`resize()` just picks up the new size; the next ordinary
frame erases from the parked cursor). Every text line goes out unwrapped
(`flowReflow` and `flowWrap` alike) so the terminal owns its wrapping, which is
why `UserEcho` commits the submitted message as `flowReflow` rather than
pre-wrapping it. The prompt is echoed at **submission time** (the driver's select
loop, before any session-store write), not when a turn starts — so the typed line
lands above the input instantly instead of waiting on MCP loading, ref expansion
turn admission. Only prompts echo; `/commands` and `!shell` lines do not. Structural fidelity on resize is alt mode's job: it owns a
viewport and re-lays everything from retained lines.

The live preview must be refreshed **before** those blocks are committed: a
completed block still sitting in `r.live` would otherwise be redrawn by
`renderer.commit`'s stale-block pass as a ghost *below* the new history. When
the screen is full that oversized ghost overflows and prematurely scrolls the
just-committed lines into terminal scrollback before they are read — output
visibly jumps during streaming. So `Text`/`EndText` call `repaint()` (dropping
the completed content from the preview) ahead of `writeMarkdown`, never after,
and `Thinking`/`EndThinking` do the same ahead of `commit`: both previews must
drop the content they are about to commit before `commit`/`writeMarkdown` runs, or
the inline renderer redraws it as a stale ghost.

**4. All public `UI` methods take `u.mu`.** Renderers are not independently
thread safe. Input runs on its own goroutine, as does the spinner ticker, and
both mutate through the same lock.

**5. Teardown runs on every exit path.** `Close` is idempotent and must run on
normal exit, Ctrl+C, Ctrl+D, SIGTERM and panic. It restores raw mode, disables
bracketed paste, and leaves the alternate screen. Alt mode also replays the
transcript onto the main screen so the session is not lost. Every long lived
goroutine starts through `UI.safeGo`, which closes the UI before a panic unwinds,
so a failure off the main goroutine cannot leave the terminal raw. That is only
deadlock free because those goroutines release `u.mu` with `defer`.

**6. Tabs are expanded at commit** (`tabSpaces`). `uniseg` measures a tab as one
column but terminals render eight, so an unexpanded tab breaks every width
calculation downstream.

**7. Taking the terminal is reversible.** Raw mode clears `ISIG`, so Ctrl+Z will
not suspend us on its own. `keySuspend` calls `renderer.suspend` to hand the
terminal back, then re-raises `SIGTSTP`; the `SIGCONT` handler in `watchSignals`
calls `renderer.resume` and repaints. Anything new that takes the terminal needs
both halves.

**8. A notice collapses only while it is still live.** Invariant 3 forbids
rewriting a committed line, so `NotifyKeyed` keeps its notice in the *live
block*, not in history. Repeating the same key rewrites that row, which is free
because the live block is redrawn every repaint anyway. The moment anything else
commits, `commitHist` flushes the notice into history first and it stops being
collapsible. This is the only form of collapse that is safe here, and it is
enough for a progress notice that updates in place.

## Interactions

Anything above the TUI can ask the user a question: `Select`, `Confirm`, `Input`,
`Pick` and grouped `MultiPick`, each with a `Context` variant, plus an approval
`OpenDecision` dialog — all blocking and all callable from a goroutine that is
not the input goroutine.

An interaction **grows the live block** rather than overlaying history, because
invariant 1 forbids inline mode from addressing committed lines. It takes the
input field's place while active, so the editor keeps whatever was typed and
shows it again once the prompt resolves. That works identically under every
renderer, which is the whole reason for the constraint — the interaction layer
only composes rows for `setLive` and touches no renderer at all.

The height cap is `maxInteractionRatio` (half the screen) rather than the input's
`maxInputRatio` (a third): an interaction is transient and modal, and a third of
a short terminal is not a usable picker. Lists scroll internally with a
`... N more` footer, which counts against the budget so row accounting stays
exact per invariant 2.

**Pick rows reserve a kind column.** A `PickItem` may carry a `Tag` (a short kind
word) and a `Mark` (the hue it takes). Every row of a list pads its tag to the
widest one present plus a space, blank tags included, so a label that draws a
tree — the rewind picker's `├──`/`└──` guides — starts at one column whatever the
row's kind. Lists with no tags render exactly as they did before the column
existed.

The rewind tree marks what is still in context by **shade**, not by a glyph: rows
on the active branch take the saturated hue and a plain body, rows off it take
the faint variant and a dim body, and the cursor row accents as always. Shade
cannot carry that under `ColorNone`, so there — and only there — the row falls
back to a `*` gutter ahead of the tag. `PickOptions.Initial` opens the list on
the current head rather than the last row, so reopening after a rewind lands
back at the same place in the tree.

Queued pending-prompt rows (`SetQueued`) sit above an active interaction like any
other live-block content: they yield first on a short terminal (activity-style)
and are driver-owned, so `Reset()` does not clear them — the steer queue re-renders.

Concurrency follows invariant 4. A blocking `Select` cannot hold `u.mu`, so the
caller registers a `pending` under the lock, requests a repaint, releases, then
waits on a result channel. The input goroutine checks for an active interaction
before the editor sees a key. Resolution promotes the queue head and repaints.
`Select` and `Pick` commit a one-line summary of the chosen row; approval
dialogs (`OpenDecision`) and answered questions (`Ask`) echo nothing, because
permit's barrier logs its own descriptive outcome notice (see below) — echoing
the prompt plus label here would duplicate it. The same duplication rule lets a
`Pick` opt out via `PickOptions.Silent`: `/model` sets it so the picker does not
commit its own summary when `SetModel` immediately announces the chosen key.
Interactions **queue in arrival order**
rather than being refused, because parallel tool calls will each want to ask
something and denying them for being simultaneous is the wrong default.
`pending.resolve` is a `sync.Once`, so a cancellation racing a keystroke settles
on whichever arrived first — it reports whether this call won, and only the
winner commits its summary or dequeues (see `routeKey`) — and `Close` resolves
everything outstanding with `ErrCancelled` so no caller is left blocked
(invariant 5). Only a resolution **with a nil error** commits: a cancelled
interaction records nothing, because the row under the cursor is not a choice the
user made (Esc on a `Confirm` would otherwise write `Yes` into history while the
caller got `ErrCancelled`). `questionState` is the deliberate exception — Esc is a
normal answer there, resolved with a nil error, so `question declined` is still
recorded. For the same reason `wait` returns whatever `resolve` settled on rather
than assuming its own branch won: a `select` whose ctx and answer are both ready
picks at random, and the answer must survive. When more than one prompt waits, a dim `+N waiting` line sits
below the active interaction, reserving its row from that interaction's height
budget (caret position unchanged because it rides below).

An **approval dialog** (`OpenDecision`) is an interaction with a caller-held
handle: `Wait` blocks for the answer, `Resolve(index)` settles it from the
caller, and `Close` abandons it so `defer d.Close()` is always safe. The first
to resolve wins — whether that is a keystroke or an external resolver (the permission
classifier or a mode cycle) — by writing the result into `decisionState` under
`u.mu` before calling `resolve`, then committing only if it won; the loser reads
nothing. The subject is shown above numbered options, elided to at most
sixteen lines and 480 characters — except the first line, which always survives
however long it is, so a single long command is never dropped whole. Subject
lines **wrap** to the width rather than being clipped: what is approved has to be
readable in full. Whatever still does not fit the height budget is reported by a
dim `… +N lines` marker, which takes a row from the subject when there is no
spare one, since an incomplete subject must never look complete. The subject is
**never** passed through `renderMarkdown`: tool output belongs in the trap-free
`Output` path (see Traps). Number keys select directly up to `interactionCap`,
arrows/Enter take the highlight, Esc cancels — returned as `ErrCancelled` so the
caller decides what it means (for approval, deny). Where no live block exists
(plain mode) the handle's `Wait` reports `ErrNoUI`, and a caller can still
resolve or close it harmlessly.

Plain mode has no live block and `readLines` already owns stdin, so a prompt is
written to history and the answer is taken from the message queue. Reading the
same queue the caller reads is what makes it race free: a plain mode prompt is
only ever opened from the message loop, so nothing else is reading while it
waits. Registering a side channel instead loses the race whenever input is piped
rather than typed.

An **agent-initiated question** (`Ask`) reuses the same interaction layer with a
different payload: multi-line prompt text and either free-text entry (reusing
`inputState`'s editing keys) or a small set of offered options (mirroring
`selectState`). It queues behind other interactions in arrival order rather than
pre-empting them. `Esc` is *declined to answer* — reported as an ordinary result
(`Answer{Declined: true}`), never an error that would abort a turn. In plain mode
the existing prompt-and-read-from-the-message-queue path carries it, and against
a closed UI (no one can be asked) `Ask` returns `ErrNoUI` immediately so a
non-interactive run is never blocked.

Options are never the only way out. `Ask` appends a **chat row** ("Chat about
this") below them; taking it replaces the list with the free-text row, so the
user answers in their own words. The result is `Answer{Chat: true}` with the
reply in `Text` — kept distinct from a choice, because a user who liked none of
the options must not have their reply read as picking one. `Esc` in the reply
goes *back to the options*; only `Esc` on the list (or `Ctrl+C` anywhere)
declines. Plain mode has no row to type into, so any line that is not an option
number is the chat reply; an empty line still cancels.

The question's prompt rides above the answer row in the live block. Prompt text,
options and the typed reply all **wrap** to the width instead of being clipped: a
question the user cannot read is one they cannot answer. Option continuations
align under the label, reply continuations under the prompt glyph like the input
row. All three stay inside the interaction height budget — the prompt takes at
most every row but one and ends in a `… +N lines` marker when it overflows, the
option window grows out from the cursor by whole options while they fit (the
`... N more` footer costs a row), and an over-long reply shows its tail, where
the caret is. The placeholder (`answer…`, `message…` in the chat row) marks where
the reply is typed.

An **approval dialog** resolution reports nothing to history by itself; the
caller (`permit.Barrier`) commits a descriptive notice instead — "Tool call
allowed this time", "Tool allowed for session" or "Tool auto allowed". That
keeps one source of truth for what happened and avoids duplicating the dialog's
prompt-and-label echo.

A lone `Esc` is indistinguishable from the start of a longer escape sequence
until more bytes arrive or enough time passes, so `inputReader.run` holds it for
`escTimeout` before reporting `keyEscape`. Without that there is no cancel key
at all. The timer is **not armed** while an in-progress paste sits in the buffer,
since a paste body can legitimately stall mid-arrival; and when it does fire on a
truncated sequence, the whole remaining buffer is dropped rather than re-decoded as
runes — only a genuine lone `Esc` (buffer length 1) is reported.

A paste that reaches `maxPasteLen` (4 MiB) without a terminator delivers the body
it has (`key.partial`), and the reader then stays **inside** the paste: the tail is
dropped up to and including `ESC[201~`, keeping the few bytes that could still
begin one, and the escape timer stays disarmed so a split terminator survives.
Decoding that tail as ordinary keys is what let a `\r` in a pasted file submit the
prompt mid-paste.

A closed input stream emits no editing keystroke. Only the literal Ctrl+D byte
(`0x04`) decodes to `keyEOF`, so an external EOF never races with typed text or
mutates the buffer; readers stop on the channel close alone.

## Wrapping policy

Every committed line carries a `lineFlow`, set by whoever produced it rather than
inferred from its text:

- **`flowReflow`**: prose. Emitted as one long logical line. In inline mode the
  terminal wraps and reflows it; in alt mode `wrapLine` handles it with a zero
  hang. Headings, paragraphs and thinking output.
- **`flowWrap`**: carries alignment (leading spaces, list markers). In inline
  mode it is emitted as one logical line exactly like prose: the terminal wraps
  it flush-left, reflows it on resize in both directions, and selections carry
  no fake continuation indents.
  In alt mode `wrapLine` breaks it on word boundaries and indents continuations
  to align under the text, since alt owns the re-layout on every resize. Code
  blocks, diffs, list items, blockquotes and raw tool output.
- **Tables, thematic breaks and dividers travel as intent, not text**
  (`histLine.table`, `histLine.rule`, `histLine.divider`) and are laid out at the
  width in force every time they are drawn, so alt mode re-lays them on resize.
  A table or break nested inside a
  list or quote cannot carry intent through the enclosing block's text, so it
  is laid out once at commit width and merged into the block's `flowWrap` lines.

The trade: inline gives every text line to the emulator, so it reflows in full
form and copies cleanly, but an overflowing continuation wraps flush-left — an
indent on the continuation would require us to choose the break point, which
would freeze the line at commit width and fragment copies. Tables and rules are
the exception: wrapping a table garbles it outright, so they keep the width
they were laid out at until they scroll away (drawn one column short of the
edge, the same deferred-wrap precaution as live rows). Alt mode keeps the
hanging indents because it re-lays everything itself on every resize.

The flow travels with the line because guessing it from the text was wrong:
prose legitimately starting with `-`, `+`, `@` or a box drawing rune was
misclassified and silently stopped reflowing.

Every `histLine` is a single logical line: each renderer writes one row per
line, so an embedded newline would silently corrupt the layout (displayWidth
measures `\n` as zero columns). Producers split already; `commitHist` enforces
the invariant by construction (`splitHistLines`) so a future producer cannot
break it.

`wrapLine` operates on `cell` values (grapheme cluster + active SGR state), so it
never splits a cluster, never miscounts an escape sequence as width, and reopens
the active style on each continuation row. This is why emoji with modifiers and
ZWJ sequences survive wrapping intact.
Table cells reuse the same `cell` machinery (`wrapCellLine`) so wrapped cell text
keeps its styling; column widths come from content and are shrunk (widest first)
when they would overflow, never dropping data. Rows get a separator line between
them.

### The output head (`output.go`)

Tool output reaches history through one mechanism shared by streamed `bash`
output and a finished tool's `Display`, so both render identically instead of
one flooding scrollback while the other shows nothing. An `outputHead` commits
only the first `outputHeadLines` (4) whole lines; everything past it is counted,
not shown, and collapses into one dim indented summary row (`… +N lines, X chars`)
at call end. The head uses a `lineBuffer`, so an escape sequence or partial line
is never split across the boundary.

- **Streaming** (`UI.Output`) feeds the head incrementally; while more than the
  head is pending it also refreshes a transient keyed activity row
  (`outputKey`+call id: `bash · N lines · B`, naming the call's own tool), so long
  output still shows movement.
- **A finished tool** sets `ToolResult.Display`, which the `ToolStart` done hook
  runs through a throwaway `outputHead` for the identical head-plus-summary
  treatment. A tool must therefore either stream or set `Display`, never both.
- **The head belongs to one call, keyed by its id** (`UI.runs`, a `toolRun` per
  in-flight call), not to the UI and not to a turn. Every entry point carries the
  call id: `ToolStart(id, name, label)`, `Output(id, delta)`, `SetOutputFull(id)`,
  and the done hook closes (flush + summary) only its own call before committing
  its result. One shared head was wrong the moment two calls overlap, which is not
  hypothetical: a staged `!`/`!!` shell streams from the `Stager`'s goroutine while
  an agent turn runs, so each `ToolStart` reset the other's stream mid-flight and
  the first done hook closed the survivor's head.
- `EndOutput` at turn end is the safety flush for the calls a turn owns, and it
  **skips full-mode runs**. `Flush` never waits for an excluded `!!` run, so one
  can still be streaming at `TurnEnd`; closing it there would commit a collapse
  row mid-stream and reopen the tail as a second, nameless, capped head. A full
  run has no collapse row or activity row to clean up, its own done hook flushes
  it, and leaving it open is what keeps its name in the status bar while it runs.
- The status bar names the newest **named** running call (`UI.toolLabel`) and keeps
  its glyph animated until every call has ended, so one finishing does not blank the
  label of another still running, and a run created by output arriving ahead of its
  header does not blank it either.
- **Full mode**: user-initiated `!`/`!!` shells are the one exception to the
  head-plus-summary rule. The stager opens them through `Sink.ToolStartFull`
  (an optional capability it type-asserts on its sink), which calls `SetOutputFull`
  for the call id *before* `ToolStart`, so output racing the header is never capped;
  the head is created on demand by whichever arrives first. With `outputHead.full`
  set, every line is committed to history and no summary or activity row appears —
  the human sees everything they ran. The flag lives for one call: ending it drops
  the `toolRun`, so an agent bash call alongside it truncates normally.

The model still receives the full unmodified `Content`; only history is elided.

## Content rendering

### Markdown

`goldmark` parses (with the GFM extension); we walk the AST ourselves in
`markdown.go` rather than registering a `NodeRenderer`, which gives control over
indentation and lets table nodes be collected into `mdTable` for
`layoutTable`.

Glamour was rejected deliberately: it pulls goldmark + chroma + lipgloss for
about 20-30 modules, and it hard wraps to a fixed width, which fights the
requirement that the terminal reflow prose.

Supported elements:

| Block | Rendering |
|---|---|
| Heading | dim `#` markers kept, text in heading style |
| Paragraph | unwrapped, soft breaks become spaces so it reflows |
| Fenced / indented code | two space indent, dim language label, syntax highlighted where the palette and colour depth allow, otherwise the flat code style |
| Blockquote | `▏ ` prefix, dim italic body |
| List | `• ` bullets or `N. ` ordered, nested by indent, tight and loose |
| Task list | `[x]` / `[ ]` checkboxes |
| Thematic break | rule drawn to the width in force, one column short of the edge |
| GFM table | our own layout: cells wrapped to their column, rows separated, re-laid out at each width (alt mode) and terminal-reflowed (inline) |
| HTML block | passed through dim |

| Inline | Rendering |
|---|---|
| Emphasis | level 1 italic, level 2+ bold |
| Code span | code style |
| Strikethrough | strike attribute (GFM) |
| Link | styled text plus dim `(url)`, suppressed when they match |
| Autolink | link style |
| Image | `[image: alt]` dim |
| Hard line break | preserved; soft break becomes a space |
| Raw HTML | passed through dim |

Nested inline styles are handled with a style stack: closing an inner span emits
a reset then reopens the ancestors, so bold containing a code span does not lose
its bold on exit.

Fenced code is syntax highlighted. See "Syntax highlighting".

### Syntax highlighting

`highlight.go` lexes a fenced block with `chroma` and formats it with chroma's TTY
formatter, so the colours arrive as SGR inside `histLine.text` exactly like every
other role. No renderer knows this happened: highlighted code follows the same
"history keeps the bytes it was written with" rule as the rest, including the
forward-only restyle limit.

Which style is used is the palette's choice (`Theme.CodeStyle`), so highlighting
tracks the theme rather than fighting it. The rules that must hold:

- **`Theme.Code` is the fallback, and it must stay reachable.** Highlighting is
  skipped for an empty `CodeStyle` (so below `Color256`), an unlabelled or indented
  block, a language chroma does not know, a lexer that colours nothing (`text`,
  `plain`), and a block whose highlighted row count does not match its source line
  count. Every one of those renders exactly as it did before highlighting existed,
  which is what keeps `TestCodeBlock` and the `ColorNone` goldens honest.
- **One row in, one row out.** Row accounting is exact (invariant 2), so
  `splitStyledLines` closes and reopens the active SGR at every line break: a token
  spanning lines (a raw string, a block comment) must not leave a row without its
  own styling, and a newline must never sit inside a styled span.
- **Token backgrounds and the style's base foreground are stripped**
  (`stripDefaults`). A block carries no shade of its own, so a token background
  reads as a stray band; and dropping the base foreground leaves punctuation and
  whitespace unstyled, which keeps code at prose weight and spends escape bytes
  only where a token is genuinely coloured. Chroma re-inherits a cleared entry from
  the parent style, so each rebuilt entry sets `NoInherit`.
- **The live preview is not highlighted** (`renderPreview`). An open fence is
  re-rendered on *every* delta, and chroma's lexer costs ~100x goldmark's parse, so
  highlighting there would put milliseconds on each keystroke-scale repaint — and
  it would churn colours as truncated strings and identifiers resolve. Highlighting
  runs once, at commit, when the fence closes and the content is final.

### Diffs

`go-udiff` computes the change; `diff.go` renders it **git shaped**: a file
header (`path +N -M`), then one `@@ -a,b +c,d @@` hunk per changed region with
`contextLines = 3` of surrounding code, matching git's default. Whole-file
rendering was tried and reverted — reprinting a large file around a two-line edit
buries the change it is meant to show.

Every row carries a right-aligned line number and a ` `/`-`/`+` marker;
deletions keep their old-file number, everything else numbers as the new file.
The gutter is sized from the whole file, so it does not jump between hunks. Hunk
headers follow git in dropping the count when a side spans one line
(`@@ -1 +1 @@`).

Changed lines are marked by **foreground colour and the marker only** — green
additions, red deletions, dim context, and `DiffHunk` cyan on the `@@` markers so
a hunk boundary reads as a separator rather than more content. That is four
distinct roles on screen at once (add / del / context / boundary), which is why
the hunk colour is its own hue and not a shade of the dim used for context. No
background shading: a shade behind every changed line reads as noise, and it
forced padding every line in a run out to a common width to stay rectangular.

Paired deletion/addition runs additionally get **intraline** emphasis: a
character level diff marks the changed spans in reverse video, applied only when
the two lines are at least 50% similar so unrelated rewrites are not turned into
confetti. Adjacent spans within a few characters are merged to avoid speckling.

**A diff is committed before its call is vetted, not after it applies.**
`guardedTool.Execute` (`pkg/tools/registry.go`) renders any `Previewer`'s
`Change` through `Output.Diff` ahead of the guard chain, so the record shows what
was requested and an approval dialog opens *below* the full diff instead of
repeating a truncated copy inside itself. `DiffSummary` gives the dialog its
one-line subject (`path +N -M (shown above)`).

Consequences that must hold: it renders once per call (before the guard loop, not
inside it); it renders in every permission mode, including the ones that never
prompt; and a denied or failed call still leaves its proposed diff in the record,
followed by the denial summary or error notice.

### Semantic styling

Meaning is carried by style, not by prefix characters, so it survives wrapping.
`style.go` defines the roles. A `Theme` crosses two choices: the colour profile
(truecolor / 256 / 16 / none) and a **palette** — a named hue table built for a
light or a dark terminal background. `NewTheme(profile, palette)` is the only
constructor, and `ColorNone` returns the zero theme whatever the palette: a palette
only ever changes *which* SGR bytes are produced, never *whether* any are.

**Choosing a profile.** `DetectColorProfile(want, env, isTTY)` mirrors `ResolveMode`,
and resolves in that order:

1. Not a TTY, or `TERM` empty or `dumb`, gives `ColorNone`. A terminal that cannot
   render escapes outranks whatever the user asked for, exactly as it does for the
   paint mode.
2. `want` when it is not `ColorAuto`. This is `ui.color`, so a user on a terminal we
   classify badly can name the depth outright — `256` to get highlighting on an
   unrecognised `TERM`, or `none` to turn colour off entirely. `ColorAuto` is the
   zero value, so an unset `Options.Color` detects.
3. A non-empty `NO_COLOR` gives `ColorNone`.
4. Otherwise env: `COLORTERM` of `truecolor`, `24bit` or `direct`, or a `TERM`
   ending `-truecolor`/`-direct`, gives truecolor; a `TERM` containing `256` or
   `direct` gives 256; anything else gives 16.

Unknown terminals stay at 16 deliberately — we never emit a depth the terminal has
not claimed, and `ui.color` is the escape hatch for anyone that costs.

**`NO_COLOR` is honoured but not documented**, and the ordering is the whole point.
It is a community convention (no-color.org) rather than a standard, and
implementations disagree on its edges — ripgrep treats any value as set, jq requires
a non-empty one — so it is not a surface worth committing to in the README. But it is
widely enough honoured that dropping it would silently re-colour the terminal of
anyone who exports it globally, and `pkg/tools` relies on the same convention
outbound: it sets `NO_COLOR=1` on every child tool process, which only works because
`rg`, `jq` and the rest read it. Sitting it *below* `ui.color` is what makes the pair
strictly more capable than either rule alone: a user with `NO_COLOR` exported can
still turn colour back on for ajent alone, which the convention cannot express. Do
not "clean up" the read as unexplained, and do not promote it to documented.

Eight palettes ship — `dark`, `dark-cool`, `dark-warm`, `dark-muted` and the four
`light` equivalents. `dark` is the historical palette byte-for-byte and a golden
test keeps it that way, so an existing user sees no change. Each role resolves a
`hue`: a 256 index with its 16-colour fallback, so both depths move together and
light palettes can drop basic cyan, which is unreadable on white. Attributes
(bold, dim, italic, reverse) are identical in every palette and live in `NewTheme`.
A palette also names the chroma style its fenced code is highlighted with
(`Theme.CodeStyle`), resolved by `highlight.go`. It is **empty below `Color256`**,
which is the single rule disabling highlighting for `ui.color=none`, `TERM=dumb` and
16-colour terminals alike: chroma snaps every token onto the 8 ANSI hues there,
which reads worse than the one hue the palette picked for `Theme.Code`.

Adding a role means a field on `roleHues`, a value in each palette, and a line in
`NewTheme`. A missing value would zero-fill into `38;5;0` — or a bare `0`, which
*resets* instead of coloring — so `TestPaletteInvariants` walks `roleHues` by
reflection and fails on any hue outside 16–255 / 31–36. New roles are covered by
it automatically; do not replace it with a hand-listed set.

| Role | Look | Used for |
|---|---|---|
| `Thinking` | dim + italic | reasoning, so it never reads as reply text |
| `Activity` | dim on the palette's shade (256/truecolor) | live sub-agent rows above the prompt, set apart from committed output; falls back to plain dim below 256 colors |
| `UserTag` / `Assist` / `ToolTag` | three separable hues | the leading kind word in the rewind tree picker |
| `UserTagOff` / `AssistOff` / `ToolTagOff` | the same hues, faint | rewind rows off the active branch, so an abandoned fork recedes |
| `User` | bold accent | echoed user messages |
| `Dim` | dim | tool output, status, context lines |
| `Accent` | the palette's accent hue | markers: `✻` thinking, `⏺` tool, list bullets |
| `Heading`, `Bold`, `Italic`, `Strike`, `Code`, `Link`, `Quote` | markdown inline roles |
| `DiffAdd/Del/Hunk/File` + `*Word` | diff lines and intraline spans |

**Choosing a palette.** `ui.theme` holds the name. `UI.DetectTone` classifies the
terminal background — `COLORFGBG` when the terminal sets it, otherwise an OSC 11
query with a DA1 (`CSI c`) fence behind it and a 150 ms cap — and its only consumer
is the first-run picker (`command.ThemeSetup`), which runs when no layer has chosen
a palette yet and offers the palettes matching the detected background. Tone is not
a stored mode: after the first answer the palette is a plain name in the user config
and the terminal is never queried again. Detection failure offers every palette
rather than guessing.

The OSC 11 answer and the DA1 fence are decoded into their own channels like the
DSR resize barrier. Decoding them is a correctness fix as much as a feature: an
unsolicited OSC reply used to leak its body into the editor as literal runes.

**Restyling is forward-only.** `UI.SetTheme` recolours the live block and everything
committed after it, but `renderMarkdown` bakes SGR into a `histLine`'s text bytes, so
history already on screen keeps the colours it was written with until the next start.
Storing roles instead of bytes would fix that — a renderer-wide change for content the
user has already read, so the `/settings → Theme` row states the limit instead. A
resumed session applies its palette *before* `session.Replay`, so restored history is
baked with the palette that session chose.

## Unicode

Widths and cursor movement go through `uniseg` grapheme clusters throughout, so
wide scripts, combining marks, emoji with skin tone modifiers and ZWJ sequences
are each treated as one unit of the correct width. A family emoji is one cluster
of width 2; both the precomposed and combining forms of `é` are width 1. Wrapping
never splits a cluster, and `truncateDisplay` never cuts one in half.

Every glyph we choose ourselves is pinned to the width its layout budgets for, because
a per-glyph miss on a glyph repeated to fill a row becomes a whole row of disagreement
that the one-column slack (rule 3) cannot absorb. The rule char, the markdown markers
and table borders, the context bar cells, the history markers and every spinner frame
have a width assertion in the test file beside their constant. Loops that advance by
measured width — `wrapLine`, `wrapCellLine`, `truncateDisplay`, `paintCaret`,
`editor.layout` — advance on cell count as well, so a cluster measuring zero columns
cannot stall them.

Two width exposures are known and deliberately **not** handled. `RUNEWIDTH_EASTASIAN`
is a user setting rather than a property of the text, and nearly every glyph here
(`─ ▏ ▓ ░ • · … ┌┬┐│` and the braille spinner) is East Asian Ambiguous, so honouring it
would have to come with an ASCII fallback set — measuring correctly on its own only
converts a mismeasured rule into one that genuinely wraps. The same fallback set is
what a non-UTF-8 locale would need. Both are recorded in
`docs/phases/18-handle-terminal-quirks.md` with the reasoning for deferring them.

## Life of a message

```
agent (or demo) -> UI.Text("...delta")
  -> buffer until a markdown block closes      splitCompleteBlocks
  -> renderMarkdown: goldmark AST -> []histLine  markdown.go
  -> UI.commitHist: expand tabs, sanitize rows, track section gaps
  -> renderer.commit([]histLine)
       inline: erase live block, write lines, redraw block below
       alt:    append to buffer, re-render the frame
```

The live block is rebuilt separately by `UI.repaint`, which composes
`notice? + thinking* + streaming* + search? + completion? + activity* + queued*
+ (input | interaction) + status rows` and hands it to `renderer.setLive` with
the caret position. Anything that changes the input, status, tool or activity
state calls `repaint`. Queued pending-prompt rows (`SetQueued`) render after
activity and yield like them on a short terminal; they are driver-owned, so
`Reset()` deliberately leaves them in place for the steer queue to re-render.
When a markdown block completes mid-stream, `Text`/`EndText` repaint **first** so
the just-committed rows leave the preview before `writeMarkdown` runs (invariant
3) — otherwise the inline renderer's commit pass would redraw them as a stale
ghost and push fresh output off screen.
Thinking follows the same shape: completed logical lines still commit to history
exactly as before, while the pending partial line renders live above the reply
preview, re-wrapped at width (raw text, not markdown-rendered), bounded by the
`thinkingPreviewRunes` cap and yielding by the same room rule that keeps only the
tail rows. `UI.EndThinking` drops the preview before committing its remainder,
and `TurnEnd` in `pkg/tui/sink/sink.go` flushes an unterminated block so an
interrupt cannot strand a tail.
Activity (and queued pending-prompt rows) render into whatever height remains
after the status block and one line of editor, so on a short terminal they yield
first. Status is computed before those budgets so row accounting stays exact.

### Finding the live block again after a resize

Inline mode redraws by erasing the live block and writing it again, which means
it has to find the block's first row. Committed rows are never part of that: they
belong to the terminal and are never re-rendered (invariant 1), so the only
question is where the block starts.

The answer is: **the cursor is already there.** Every draw ends by parking the
cursor on the block's first row, so `eraseLive` is nothing but `\r` +
`eraseBelow` — return to the start of this line, clear downward. There is no row
arithmetic in the erase at all, which is what makes it immune to the failures
that produced this bug repeatedly:

- the emulator reflowing the block into a different number of rows than we
  predicted (every emulator reflows a little differently, and some not at all),
- a glyph the terminal measures wider than `displayWidth` does, so a row spans
  two rows and the count is short,
- a resize landing between composing a frame and the terminal consuming it.

Each of those used to leave the block's top row behind, once per miss — and the
misses accumulate, because the stranded row is committed terminal content by the
time anyone could notice. A reflow keeps the cursor on the first cell of its
logical line, and the first cell of the block's first row is exactly where we
parked, so none of it matters any more.

The following rules keep that true:

1. **The caret is painted, not parked on.** The terminal's own cursor is hidden
   for the whole session; `paintCaret` reverses the cell the caret sits on as the
   row is written. That is what frees the cursor to sit where the *erase* needs
   it rather than where the *user* needs it, and it is why the erase can be
   position-free. Both renderers do it, so the caret looks the same in either.
2. **Every live row is exactly one terminal row.** Row text arrives from callers
   (`SetActivity`, `NotifyKeyed`, a tool label), and tool progress in particular
   is arbitrary text that may carry newlines, tabs or escape sequences.
   `sanitizeRow` folds line breaks and tabs to single spaces, drops the
   remaining C0/DEL/C1 controls, and keeps only complete non-private SGR from
   the escapes — a cursor-motion or screen sequence moves the cursor in ways no
   row count predicted (the park lands inside the block) and a truncated escape
   would swallow the park sequence as parameters. Keeping SGR is why styled tool
   output still reads, and it runs at every boundary: `composeRows` folds each
   live row, `commitHist` sanitizes committed lines once, and the public setters
   (`SetActivity`, `NotifyKeyed`, `ToolStart`, status) do too. Zero-width escapes
   are exactly why this is row accounting rather than cosmetics.
3. **The live block never fills the last column.** `repaint` composes every row
   one column short, and activity rows carry an extra spare column of slack on
   top (`shadeRow` pads to `w-1`, reserving measurement room). A row ending in
   the last column leaves the cursor in the deferred-wrap state, and emulators
   disagree on whether the next byte lands on the same row or the next one —
   which would put the park a row out. Composing narrow rather than truncating at
   draw time means nothing is cut off the editor or a dialog; the spare column
   converts the most likely width disagreement (`uniseg` vs the terminal) from
   corrupting to reflow-ambiguous.
4. **The park counts only what it is writing, at the width in force now.**
   Parking still has to move up over the rows just written, so `composeRows` sums
   them — but only rows it is emitting itself, never rows the emulator may have
   reflowed since. It re-reads the size after composing them, so a resize that
   landed mid-frame is accounted for before the count is taken; a diff frame
   counts each skipped row as exactly its one newline boundary (it descended no
   further), which returns the cursor precisely to where the previous frame
   parked. And the write itself is gated, by two things that must **both** be
   raised without waiting on `u.mu`. `watchSignals` raises them the instant a
   SIGWINCH arrives, before it contends for the lock: `resizing` (an
   `atomic.Bool`, so `repaint` returns and `commitHist` defers) and `sigGen`.
   A frame is then judged against `drawGen`, the generation the settled redraw
   last caught up with — **never** against a generation captured as the frame
   starts. Both halves are load bearing, and each was wrong once:

   - A gate taken under `u.mu` is not a gate. A streaming `Text` holds the lock
     across a goldmark parse (and a chroma highlight on commit), so
     `holdForResize` sits queued behind it for milliseconds while that call
     composes and lands a whole frame on a grid the emulator is already
     reflowing.
   - A per-frame baseline is not a baseline. Composing parses the open markdown
     block, so a signal landing mid-compose becomes that frame's own generation
     and `stale` compares equal — the frame writes anyway. Comparing against
     the settled generation makes any unsettled signal abandon the frame,
     whenever it arrived.

   Together these narrow the mid-reflow window to syscall duration. The gate
   itself is generation-checked when the settle clears it: `sigGen` bumps
   before `holdForResize` takes the lock, `holdGen` records the generations
   sequenced under it, and a settle only clears while they match — otherwise a
   signal that bumped but has not reached `holdForResize` would be absorbed
   into `drawGen` and a frame could land mid-reflow with neither gate raised.

   One window remains and is accepted: between the emulator finishing its
   reflow and our SIGWINCH being processed, a streaming frame or commit can
   still land, erasing from wherever the reflow moved the park. If the park
   was clamped above visible history that erase destroys it — unrecoverable,
   since inline never re-renders committed rows. Nothing can close this
   without predicting signal delivery; the settle's CPR probe runs *after* the
   damage, so the underfill test (rule 8) at least pads the block back to the
   bottom instead of leaving it stranded mid-screen.
5. **The live block never exceeds the screen.** All of the above assumes the
   block is erasable, and a block taller than the screen is not: drawing it
   scrolls, so the previous frame's top rows are pushed into scrollback where no
   erase can reach them — one stranded copy per redraw, compounding, which is
   what a long reply streaming into a short terminal used to do. Every producer
   budgets itself against the rows left, and the streaming preview yields
   hardest because it is the only one whose height follows the content rather
   than the terminal: it keeps its tail, which is what a reader is watching.
   Dropping the head is **marked** — a dim `•••` on the preview's first row,
   taken out of the preview's own budget so the total is unchanged. Unmarked,
   an open block taller than the screen looks like the committed line above it
   is eating text: the preview sits above the divider, so it reads as
   transcript while it is in fact redrawn every delta. The marker is the same
   admission `+N more` and `… +N lines` make elsewhere, and it is preview-only —
   the block commits whole (head included) the moment it closes.
   `repaint` then clamps the total as a last line of defence, dropping from the
   top and carrying the caret with it, so a floor (search, completion) or an
   unlucky width cannot overflow either (`TestUILiveBlockFitsTheScreen`). A block
   clamped to exactly the screen height leaves alt no history rows at all, which
   is what `full_height_block_keeps_every_row` pins.
6. **The settled redraw waits on a terminal barrier.** A burst holds drawing
   back for as long as signals keep arriving — never redrawing mid-drag, no
   matter how long it runs: a frame emitted while the emulator is reflowing is
   exactly the frame that strands a row, and a frozen live block during a drag
   is cheap because the emulator is busy mangling the screen anyway. But even a
   quiet signal stream is not proof the grid is stable: the ioctl reports the
   new size before the emulator has finished reflowing to it. So the settled
   redraw clears two barriers before a byte goes out:

   - `probeResize` emits a DSR status query (`CSI 5 n`) and waits for the
     reply (`CSI 0 n`, decoded as `keyStatusReport`): the terminal processes
     input strictly after the resize that preceded it, so the reply proves the
     reflow finished. A `resizeProbeTimeout` grace releases the barrier on
     terminals that never answer.
   - `drawSettled` then waits one more quiet grace (`resizeDrawGrace`): the
     reply cannot cover a resize that starts *after* it was sent, and a
     SIGWINCH can reach a busy goroutine late, so a frame only goes out when
     no new signal arrived during the grace. During continuous fast resizing
     every grace is invalidated, so no frame is emitted until a genuine pause.

   Both barriers re-check the `resizeSeq`/`probeSeq` generations, so a reply
   or timer belonging to an older burst never releases a draw. `Close` flushes
   whatever is still in `UI.deferred`, so a burst overlapping the end of a
   turn cannot swallow committed output.
7. **The live block diffs against its previous frame.** A 90ms spinner tick was
   reprinting the whole block; now unchanged rows are skipped, writing only the
   newlines that walk past them and an erase-to-end-of-line after each written
   row, so a tick emits no editor bytes. The diff falls back to today's full
   erase-and-redraw on five guards: a width change (the one thing that reflows
   rows it did not write), a row-count change (what the single erase-below used
   to cover), nothing drawn yet or an invalidation set by `commit`/`suspend`/
   `resume`/`clearHistory` (or by `reanchor`, whose pad needs the full path),
   and every 64th frame as a cheap safety net. Comparing
   post-caret strings means a moved caret dirties both its old and new row.

   The severity asymmetry is why this is acceptable: a stale row inside the live
   block sits in erasable territory, healed by the next commit or full redraw,
   whereas a stranded row above the block is committed content nothing can reach.
   Diff staleness is self-healing; stranding is permanent.
8. **The park is ground truth until a reflow moves it off the block's top.** Every
   rule above assumes the parked cursor still marks the block's top. That holds
   through any reflow *of the block itself*, because the cursor rides its cell.
   A shrink breaks it: a narrowing rewraps history into more rows, a shorter
   screen has fewer rows to hold it, and either way the block's top — the parked
   cursor itself — can retire into scrollback, leaving the terminal to clamp the
   cursor onto what is left. The block then ends mid-screen with space below it
   that no erase reclaims. The terminal knows where the cursor really is, so the
   settled redraw asks: `probeResize` writes a cursor position report query
   (`CSI 6 n`) just ahead of the DSR status query that already releases the
   barrier, and `settleResizeLocked` consumes the reply (`CSI row;col R`,
   decoded as `keyCursorReport`) after `resize()` and before the repaint. When
   the reported row plus the block's rows ends above the last screen row on a
   started session, the renderer `reanchor`s and the next full draw pads the
   block back to the screen bottom.

   The pad respects the two invariants this section is built on. CPR is a
   *read*, and invariant 1 forbids *addressing* rows we did not write: the
   query addresses nothing, and the pad is newlines only. It is measured from
   the reported row (`height − live − (row − 1)`), which is what keeps it from
   scrolling — `reanchor` rejects any row whose block already reaches the
   bottom, so the cursor lands on exactly `height − live` and the block fills
   the rows below it, every one of which `eraseLive` has just cleared. **The pad
   never displaces a committed row.** Measured from `height − live` alone it
   overshoots by the park's own row and scrolls that many rows away — on a
   session whose history does not fill the screen, all of it
   (`TestUIReanchorKeepsHistoryOnScreen`, `pad_never_scrolls`). A garbage or
   hostile row stays bounded: the underfill test rejects anything at or past the
   bottom and the repeat is clamped to the screen, so a bad row can only fail to
   mean underfill, never pad past one screenful. The probe is additive: the
   status barrier (rule 6) is untouched, and a terminal that answers DSR but not
   CPR degrades to the pre-CPR behaviour.

   Only a shrink re-anchors. A settle that grew in one dimension and shrank in
   neither cannot have retired the park — a grow rewraps into fewer rows and
   takes none away — so its dead band is cosmetic, and bottom-anchoring a
   maximized window would drop the block a screenful for a repair nobody needs.
   Both dimensions count: a corner drag that widens but shortens is a shrink,
   and an equal-size settle re-anchors too, because its burst may have narrowed
   and been dragged back (`TestUIReanchorGestures`). The reply must also be one
   this settle asked for (`cprPending`): SIGCONT and a direct `resize()` settle
   without probing, and a report left over from an earlier burst is no longer
   true. The flag proves a probe of ours was in flight, not that this reply is
   the one answering it — replies carry no identity — so a superseded reply the
   reader decodes after this probe's drain still passes for fresh. Reports are
   drained on every settle either way and the newest wins the channel
   (`sendLatest`), which leaves that residual bounded by the same rules as any
   wrong row: one screenful at most, and nothing at all once the block reaches
   the bottom.

   A pad lives exactly one draw. `reanchor` sets its outcome on **every** path,
   rejections included, and every settle calls it — passing a row of zero when it
   has no usable evidence — because a pad can outlive the frame that should have
   consumed it: `paint` abandons a frame a signal raced (rule 4), and the flag
   survives with it. Applied a gesture later against a grid that has moved, that
   pad is the overshoot the row exists to prevent: a park already at the bottom,
   padded from row 1, scrolls a screenful of committed history away
   (`reject_clears_a_pending_pad`, `TestUISettleClearsPendingPad`). `commit`,
   `suspend` and `clearHistory` clear it for one reason between them: each moves
   the block, or the screen under it, out from beneath the row the pad was
   measured on. Dropping a pending repair costs a block left stranded until the
   next probed settle — cosmetic, and output walks it back down — where keeping
   one costs displaced history. The pad itself is measured against the frame it
   lands on rather than the one the settle inspected, since `repaint` recomposes
   the block in between and only the height being drawn now puts its last row on
   the last screen row (`pad_follows_the_frame_it_lands_on`).

   The underfill test trades a bounded false positive: a session whose
   committed history is shorter than the screen also reports underfill, so a
   shrink bottom-anchors it and leaves a blank band above the block until
   output fills it. Counting history's real rows would mean predicting reflow,
   which invariant 2 forbids; the band is blank rows only, displacing nothing,
   and it scrolls away. Nor is a clamp distinguishable by the reported row
   alone — a real overflowing narrow reports whatever row the surviving lines
   left, not row 1. Because the probe follows every write that preceded it, the
   same test repairs a frame or commit that slipped through the signal-delivery
   window (below).

   This fixes the on-screen anchor only. The rows the same reflow retired into
   scrollback stay unreachable: inline mode cannot erase scrollback without
   destroying the whole session, and that is the accepted price of native
   scrollback (out of scope).

### Why inline does not re-render committed history on resize

The relative erase keeps every rule above true: on a settled size change,
`resize()` just picks up the new size (`refreshSize`) and the next ordinary frame
erases from the parked cursor and redraws the live block at it. That is all inline
does, and it is enough for everything interactive — input, status, dialogs,
tool progress — because those all live in the live block whose top is the parked
cursor.

It deliberately does **not** re-lay committed history (code, lists, quotes, diffs,
tables, rules) at the new width. Three designs tried to, and each corrupted a real
terminal:

1. A full viewport redraw from `cursorTo(1,1)` destroyed whatever was above the
   session.
2. An absolute `repaintScreen` that rewrote visible rows with `cursorTo(row,col)`
   worked at the bottom of the screen but corrupted scrollback as soon as the
   terminal was scrolled up — an absolute write lands on whichever rows are
   currently displayed, which may not be ours.
3. A relative climb from the parked cursor bounded by a count of emitted rows fixed
   the scrolled case but broke on **widening**: real emulators pull scrolled-off
   rows back onto screen when content shrinks (soft prose rejoins), while our own
   hard-broken structural rows do not rejoin — so a row-count computed from the new
   layout never equals how many physical rows separate the parked cursor from our
   content top. The rewrite overlapped surviving old rows and duplicated them.

The lesson is load-bearing: **we cannot know where committed rows sit after an
emulator reflow.** Only the live block's top — the parked cursor, which the terminal
tracks through reflow — is ground truth we can rely on. So inline leaves every
committed line exactly as it landed (invariant 3) and never rewrites one.

The way structural content still gets full-form fidelity is to hand the wrapping to
the emulator in the first place: **every** text line — prose, code, lists, quotes,
diffs, tool output — is emitted as a single logical line, so it reflows on resize
like `cat` output and comes back whole when widened (and copies as one line).
Only genuinely two-dimensional content (tables, rules) keeps our hard layout —
and keeps its committed width until it scrolls away (the seam). That is the
price of never corrupting: we touch only what the emulator can reflow for us.
Alt mode exists for full resize fidelity — it owns a viewport and re-lays everything from
retained lines (`histLine.rows`), including thematic breaks now that `markdown.go`
retains rule intent rather than baking width in.

### Rewind and resume replay the branch, not erase scrollback

The other place committed rows look like they move is a **rewind** (double-Esc onto an earlier
context-tree point) or a **resume** (`--continue` / `--resume`). Both route through
the same manoeuvre in the front end: rebuild agent state from a branch head, then call
`ui.Reset()` and drive `session.Replay(branch, tuisink.New(ui))`. Two distinct things
happen, and they must not be conflated:

1. **Nothing above is erased.** Inline's `clearHistory` emits only `\r` + `eraseBelow`
   from the parked cursor (near the bottom of the viewport when content fills it), so it
   clears the live block, never committed history or native terminal scrollback. Rows that
   have scrolled off remain untouched and fully intact — scrolling up still shows them.
2. **The restored branch is re-submitted as fresh committed lines.** `session.Replay` walks
every entry in the branch and emits sink events (`TurnStart`, `UserPrompt`, `Text`,
`ToolStart`, ...) which render *again* below whatever survived. So a rewind does not
replace scrollback; it appends a second, condensed rendering of the restored context.

The front end commits a **divider** — one solid full-width band (`ui.Divider()`) in the
theme's `Divider` style (reverse video) — *before* replaying, so where restored history
begins is obvious when scrolling up past it. It is committed on a **rewind**, not at startup:
a resumed session opens onto a fresh screen whose only content is that same restored branch,
so there is nothing above the replay to mark off; a rewind lands below already-committed rows,
where the boundary must be visible. The divider is a `histLine.divider`, drawn to the width
in force like a thematic break; with color disabled it falls back to a thin rule.

The result is that scrolling up reads continuously — because nothing was deleted — while
the live area shows a fresh copy of the branch. This is why a restore can look both
"unchanged above" (native scrollback) and "rebuilt below" (the replay). The two layers are
independent.

Replay rendering differs from live output in ways that matter for fidelity:

- **User prompts render their words.** `session.Replay` emits each prompt's text through
  `TurnStart(Input.Text)` *and* `UserPrompt(text)`, and `tuisink.UserPrompt` routes it to
  `ui.UserEcho` — the same path a live session uses at submission time, so restored context
  shows user prompts above their replies. The `TurnStart` itself only lights the working
  spinner; its text is carried by the separate `UserPrompt` call.
- **Thinking is dropped.** Replay opens with `ReplayOptions{}`, so `opts.Thinking` is false
  and the `llm.ThinkingBlock` branch never fires. This is deliberate — thinking reads as
  noise on a restore.
- **Tool calls render headers plus their result bodies.** Each `ToolCallBlock`
  becomes `sink.ToolStart(...)` (a header via `ui.ToolStart`) and its matching tool-result
  message is handed to that call's completion hook with the full body as its display text
  (`foldResults` → `toolBody`). The commit path runs through the same output-head / collapse
  rules live streaming uses, so only the first few lines reach history and the rest fold into
  a count summary — bodies replay without unbounded scrollback.
- **Assistant text replays verbatim** through `Text`/`EndText`, so replies survive intact.

The design intent: a restore reproduces the committed history — user prompts, tool
headers with their bodies, and assistant content — minus thinking only. Dropping thinking
is accepted as noise on a restore.

## Input

`input.go` turns bytes into `key` values: printable runes, control keys, arrows,
Home/End/Delete, PgUp/PgDn, Ctrl+Z and bracketed paste. SS3 (`ESC O <x>`) maps a
safe subset only — the four arrows plus Home (`H`), End (`F`) and keypad Enter
(`M`); `p`-`y` are deliberately absent because tcell reads them as PC-keypad
*navigation*, not digits, so mapping them could turn an inert key into a wrong
action inside a dialog. The CSI arrow modifier is parsed as tcell's raw bitmask:
Ctrl or Alt promotes to word movement (`;4 ;6 ;7 ;8` and sub-parameters included),
Shift alone does not (the editor has no selection). A bare `CSI R` is CPR here
while it is F3 elsewhere — there is no F-key type, so the parameterless branch
stays ignored. The parameterized form (`CSI row;col R`) decodes to
`keyCursorReport` on the `reports` channel, where the newest report supersedes
any older one still queued; the resize re-anchor (rule 8 under Finding the live
block again after a resize) is the only consumer, so a CPR reply can never leak
into the editor as text. It is a pure function
over a byte slice (`decodeKey`) plus a goroutine that feeds a channel, so it is
testable without a terminal.

Mouse reporting is deliberately not enabled. It would buy wheel events at the
cost of the terminal's own text selection for the whole session, so PgUp/PgDn
are the way to scroll in alt mode.

`editor.go` is the buffer. Positions are grapheme cluster indexes, not bytes or
runes, so the cursor moves over emoji and combining marks as a unit. It also owns
its own layout (`inputView`), wrapping the buffer into display rows on word
boundaries (a word moves whole to the next line rather than being split across
lines; only an unbroken token wider than a full row hard-splits) and reporting
the caret's row and column within them. Wrapping is purely visual: `Value()` is
untouched, so submitted input never gains newlines. Movement and editing keys
respect the same rows: Home and End bound the current wrapped row rather than
the logical line, matching `↑`/`↓`. Ctrl+K kills only to the end of that row
(no newline is inserted where the remainder joins), and on an already-empty row
removes it like Delete — the caret stays put as content after it joins at the cursor, or
as an empty row is removed (the row below joins, or the row above when nothing
is below). Clearing a whole wrapped row would leave the spaces both wrap breaks
dropped, so the leading one is consumed and no double space remains where the
rows join.

The key table:

| Key | Effect |
|---|---|
| Enter | submit (accepts an open menu or search selection) |
| Alt+Enter, Ctrl+J | insert a newline |
| `↑`/`↓` | move the caret through visual rows; only at the prompt's very start (↑) or end (↓) do they recall history. A search overlay or a command menu selects with them; path completion never takes them |
| Tab | accept the highlighted command in a menu; for a path, fill in the candidates' longest common prefix, or list what is left (`complete.go`) |
| Ctrl+C | clear non-empty buffer; interrupt when active; quit empty |
| Ctrl+D | EOF on an empty editor (quits) |
| Alt+↑ | recall the newest queued message into the editor — emitted as `ControlRecallQueued` |
| Ctrl+K | clear to the end of the current visual row, caret unmoved (content after it joins at the cursor); an empty row is removed like Delete (see above) |
| Esc, twice | rewind onto an earlier message while idle |
| Ctrl+R | reverse history search overlay (`search.go`) |
| Shift+Tab | out-of-band `ControlModeCycle` — never consumed by the editor or a dialog; the front end cycles the permission mode |

A paste over `pasteThreshold` (2 KiB) does not land in the editor: its content is
stored on the UI and a `[pasted N lines #K]` marker is inserted instead, expanded
back to the full text at submit (`expandPastes`). `K` is a per-session counter, so
two pastes of the same line count keep separate entries — sharing one key let the
second overwrite the first and both markers expand to the second's text. Entries
are kept for the whole session and expanded oldest first, so a recalled prompt
still resolves and nesting is deterministic; only `Close` drops them.

Keys that resolve to nothing are emitted on `Controls()` as `Control` events so
the host decides their meaning. Shift+Tab is special: it reaches the control
channel even while an interaction or overlay owns the keyboard, because changing
a permission mode with a prompt already on screen must work. Alt+↑ emits
`ControlRecallQueued`, which the front end maps to popping the newest queued
prompt back into the editor; like every non-Shift+Tab key it is swallowed while an
interaction or overlay owns the keyboard. The front end maps
it to `Barrier.Cycle()`, which re-evaluates any open approval dialog under the new
mode — moving to `allow-all` resolves one as allow without a keystroke.

**Ctrl+R opens a reverse history search** over one merged recall source — every
line typed this workspace (`/cmd`, `!shell`) plus recorded prompts, newest first,
deduplicated — drawn as an inline `(reverse-i-search)` overlay above the editor (not
a modal picker). It opens blank — nothing is shown until you type content to match
against, then typing narrows on a case-insensitive substring. Repeated Ctrl+R steps
to the next older match, Enter fills the editor with the full line and does not send
it, Esc closes leaving whatever was typed untouched. In the overlay ↑/↓ select: one
press fills the editor with the highlighted line and closes the overlay without
sending; subsequent plain arrows keep scrolling that same recalled list.

Plain ↑/↓ are **cursor-first** for multi-line prompts rather than always recalling
history. They move the caret across *visual* rows (the same word-wrapped layout
the editor renders) keeping roughly the same column, clamping to a shorter line.
Only at the buffer's edges do they touch history: on the first display row an Up
moves mid-text back to the prompt's start and only a press already sitting on the
very first character recalls older; symmetrically a Down on the last display row
jumps mid-text to the prompt's end and only a press at the very end moves toward
the live buffer. So scrolling history from an edited line takes two presses — one
to reach the start/end, one to scroll.

Recall state never short-circuits this: browsing recorded prompts is tracked by
`promptIdx`, but ↑/↓ apply cursor movement first and only fall through to
`promptPrev`/`promptNext` at a boundary. That keeps an edited or recalled multi-
line prompt fully navigable with the arrows — pressing Up after recalling fills
the caret back toward the start before it steps to the next older entry, and Down
returns newer from the end. The held-draft restore (`stashP`) still works because
`promptNext` guards on `browsingPrompts()` internally.

The recall source is shared: plain ↑/↓ (no Ctrl+R) walk the same newest-first set as
the search overlay — first ↑ recalls your most recent sent line, further ↑ steps
older and ↓ returns to the live draft. The editor's in-memory `e.history` fallback
(`HistoryPrev`/`HistoryNext`) applies only when no recall source is installed (no
session store). Like completion it is an in-place live-block overlay rather than an
interaction: the provider (`SetHistorySearch`, backed by `RecallIndex`) runs off the
key loop so a slow scan never blocks input. The overlay's pure logic lives in
`search.go` with no dependency on the UI, and the two overlays are mutually
exclusive by construction.

## The demo

The TUI is exercised by a real agent loop against a scripted model server.
`ajent-demo` (root module, `demo` build tag) points its own `AJENT_HOME` at a
temp dir and spawns the sibling `bin/ajent-demosrv`, which speaks
chat-completions SSE and plays an eleven-step script of real tool calls. Nothing is
simulated below the wire, so every renderer path runs on genuine turns.

The script is a long chain of deliberately small turns (mostly one thought or
none plus one or two tool calls) across three scratch files (`notes.go`,
`retry_test.go`, `README.md`) so reads and greps stream real contents many
times. In order it exercises: an approval dialog (`mkdir`), a `write` with a
Preview diff subject, a failing `edit` that skips its prompt via DryRun and
renders an error result, read-only auto-allow reads, a successful `edit` showing
a unified intraline diff, long thinking + `find`, more writes (`retry_test.go`,
`README.md`) each followed by grep/read of the new file, parallel dispatch
(`grep` + `read` in one message), compound read-only shell lines (`wc && head`,
`ls -la`), two streamed cats (the smaller test file, then ~180 raw lines of
`notes.go`) into scrollback, and a final dialog (`rm -rf`) before the closing
turn. The full markdown showcase (headings, bold/italic/strike, inline + fenced
code, GFM table with CJK/emoji, blockquote, rule, lists, links) runs in that last
turn, after every tool result so the text wall cannot bury it; the final line
reports the measured run time.

The demo prose lives in `demo/srv/content.go`. Two helpers matter: `unwrap` joins
source wrapped lines within a paragraph so it reflows instead of being frozen at
the Go source's width, and `expandTicks` turns `@@@` and `@@` into fences and code
spans since raw strings cannot contain backticks.

## Conventions

Repository style that this package follows:

- `var x T` for zero values, not `x := zeroValue`.
- Godocs describe inputs and outputs, not mechanism. Comments are short phrases,
  one line, only where the context is non-obvious.
- No em dashes and no non-ASCII in comments. Non-ASCII in string literals is
  fine, and necessary, since the UI renders glyphs.
- Prefer stdlib `slices`, `maps` and `strings` over hand written loops. The wider
  house style also reaches for `github.com/go-analyze/bulk`, which this module
  does not currently depend on.
- One `_test.go` per implementation file, one `Test<FunctionName>` per target
  function, table driven or `t.Run` subtests, case names three to five words in
  lower snake case. `t.Parallel()` at function level, not per case.
- `require` for setup, `assert` for assertions. Never `time.Sleep`; use
  `require.Eventually` or a deterministic trigger.

## Testing

There is no real terminal in CI, so `vt_test.go` implements a small but faithful
VT emulator: a rune grid, cursor, scroll region, correct **deferred wrap**
semantics (a rune written at the right margin does not wrap until the next
printable byte), and **soft-wrap reflow** — `vt.setSize` joins continuation
rows into logical lines and re-wraps them at the new width, retiring overflow
to scrollback, the way a real emulator does on resize. Renderer tests write
into it and assert on the rendered screen rather than on raw escape bytes,
which is what makes layout bugs visible.

The emulator models every sequence this package emits *and* the ones caller
text may carry — cursor motion (`H f A B C D d G`), IND/NEL/RI, IL/DL/SU/SD,
C1 CSI (`U+009B`), tabs to 8-column stops and zero-width attachment. This
fidelity is load bearing: a sequence the emulator silently ignores cannot be
distinguished from one that was stripped, so proving `sanitizeRow` drops motion
escapes would be vacuous without it. SGR deliberately does **not** clear a
pending deferred wrap (only cursor motion does), which makes exact-width tests
honest.

Patterns:

- Renderer tests construct a `termState` directly with `fd: -1` and a fixed size.
  Point `sizeFn` at the emulator (`return v.w, v.h, nil`) and drive `v.setSize`
  for a reflow-faithful resize; leaving `sizeFn` nil pins the size. Pointing
  `sizeFn` at a width the emulator no longer has is how the tests reproduce a
  draw that raced the reflow. The corruption regressions are built this way:
  `TestInlineRendererResizeRace` shows a frame composed at a stale width still
  parking correctly because the park re-reads the size, `committed_rows_are_never_re_rendered`,
  `leaves_content_above_the_session` and `history_is_never_touched` pin invariant 1
  against the emulator's own reflow (a resize must never rewrite committed rows),
  `TestUIActivityNewlineKeepsRowCount`, `TestUIActivityEscapeKeepsRowCount`,
  `TestUIActivityTabKeepsPark` and `TestPastedEscape*` cover rule 2 from the
  public API (a contaminated progress row, pasted escape staying byte-exact in
  the buffer), and `TestUIResizeStorm` storms resizes against an escape-laden,
  wide-glyph progress update asserting one divider, no duplicated history and a
  parked cursor at every step. Row diff behaviour is pinned by
  `TestInlineDiffSkipsUnchangedRows` (a tick emits no editor bytes) and the two
  fallback tests; `repeated_draws_never_accumulate` still drives full redraws.
  The park itself is asserted with a geometry primitive (`assertParked`, plus
  `UI.cursor`) rather than by screen content: contaminated rows must leave the
  cursor on the block's top row. Because a scrolled viewport is not
  representable in `vt`, the corruption mechanism that sank three earlier designs —
  absolute addressing into committed rows on resize — is pinned as an emitted-stream
  property: `TestInlineNeverAbsolute` drives narrow *and* wide resizes and asserts no
  cursor address sequence (`CSI r;c H`) ever appears, so inline can never land a write
  where it cannot locate its own content. The retained-intent guarantee that alt's
  re-layout relies on is covered by `TestInlineRelayoutBakesNoWidth`.
- `newTestUI` in `ui_test.go` drives a full `UI` in inline mode against the
  emulator, with an `io.Pipe` for keystrokes. Assertions use
  `require.Eventually` against a locked read of the emulator.
- For end to end checks against a real terminal, `pty_test.go` (Linux) opens
  `/dev/ptmx`, runs the UI against the slave and drives an emulator from the
  master. Almost everything there is real: `term.MakeRaw` is a real line
  discipline change, keystrokes written to the master really cross the kernel,
  `term.GetSize` really reads `TIOCGWINSZ`, and `Close` restoring the termios is
  read back with `TCGETS`. **Only the origin of SIGWINCH is not** — the slave is
  not a controlling terminal, so nothing is delivered and the test raises the
  signal on itself with `syscall.Kill`, which drives the whole real
  `watchSignals` chain (debounce, `probeResize`, the DSR barrier, the grace, the
  redraw). Kernel-originated delivery is the kernel's contract, not ours; making
  it real would need a re-exec'd child with `Setsid`/`Setctty`, which buys that
  one fact for a cross-process readiness protocol.

  The barrier is what makes the resize path assertable without a sleep: the
  emulator counts outgoing `CSI 5n` in `vt.dsrCount`, so the test waits for the
  probe, writes the `CSI 0n` reply into the master, then waits for the settled
  redraw. `eventuallyPTY` polls by pumping the master with a short read
  deadline, on the test goroutine, so the emulator is never read and written at
  once.

  `TestPTYResizeStrandsNoRow` narrows to 21 and widens to 80 asserting one
  divider, never-doubled history and the park each step; `TestPTYSignalResize`
  does the same through a real signal; `TestPTYKeystrokes` proves raw mode by
  the absence of a kernel echo; `TestPTYRawMode` and `TestPTYTeardown` pin the
  termios round trip and the `showCursor`/`bracketedPasteOff` restore.

  **No pty test may call `t.Parallel()`**: `signal.Notify` is process wide, so
  two live UIs would answer each other's SIGWINCH. Only a UI built through `New`
  runs `watchSignals`, and every such test lives in that file.

  A manual `script -qec '...' /dev/null` run plus an external `stty -F
  /dev/pts/N cols 120 rows 30` still verifies what we emit over ssh, where reply
  latency widens the window.

## Traps

Things that look fine and are not:

- **Do not use a DECSTBM scroll region.** It suppresses reflow on resize in at
  least some emulators, and under tmux a margined region may not feed scrollback
  at all. This was the original design and the source of the worst bugs.
- **Do not measure with `len()`.** Use `displayWidth` (ANSI aware) or
  `uniseg.StringWidth`. Use `graphemesOf` for cursor movement.
- **Do not send tool output through the markdown renderer.** `--- PASS` parses as
  a thematic break and `=== RUN` as a setext heading. Use `Output`/`EndOutput`.
- **Do not emit `ESC[2J` or `ESC[3J` in the main screen.** That is what destroys
  scrollback in tmux and VS Code.
- **Do not put caller text in a live row without `sanitizeRow`.** A newline or
  tab makes the terminal use columns and rows nothing counted, so the cursor
  parks inside the block instead of on top of it; an escape sequence moves the
  cursor outright. `composeRows` sanitizes every row for you; do not add a draw
  path that skips it.
- **Do not pad to a width measured on a different string than the one drawn.**
  Padding computed over raw text (`shadeRow` once did) is arithmetic on a
  string that no longer exists by draw time, because folding changes its width;
  sanitize, then truncate, then pad, so what you measure is what you emit.
- **Do not send caller text to history unsanitized.** Tool output streams raw
  bytes; `cat` of a file containing `ESC[2J` would fire the no-full-erase trap
  through caller data. Sanitize at commit (`commitHist`) keeping SGR, so colored
  tools still read and motion/screen escapes never reach the terminal.
- **Do not add an unbudgeted row to the live block.** Anything that grows with
  content rather than with the terminal must yield to the rows below it. A block
  taller than the screen scrolls as it is drawn, and scrolled rows are committed
  content that no erase can take back.
- **Do not leave the terminal cursor on the caret.** It is parked on the live
  block's first row so the erase needs no arithmetic; the caret is drawn with
  `paintCaret`. Re-showing the cursor mid-session, or moving it to the caret,
  reintroduces the whole class of stranding bugs.
- **A gutter prefix does not survive terminal wrapping.** Prose is marked by SGR
  styling only (thinking is dim + italic), never by a per-line prefix, because
  continuation rows would lose it. `flowWrap` lines may still *lead* with a
  marker (it sits at the start of the logical line), but in inline mode their
  continuations wrap flush-left — only alt mode, which owns the breaks, repeats
  the indent.
- **Inline never re-renders committed history.** Any attempt to rewrite rows
  we no longer know how to locate — an absolute `cursorTo` into them, or even a
  relative climb counted from our own layout — corrupts on real terminals
  (scrolled up, or widened so the emulator pulls scrollback back). The erase
  tests (`TestInlineRendererResizeRace`, `repeated_draws_never_accumulate`,
  `erase_needs_no_row_maths`, `TestUIActivityNewlineKeepsRowCount`) drive
  exactly one path, the relative live-block redraw; keep it that way.

## Extending

- **New output kind**: add a method on `UI` that formats, then calls `commit`
  with the `lineFlow` its layout needs. No renderer change needed.
- **New markdown element**: add a case in `mdRenderer.block` or `inline`
  (`markdown.go`), then extend `TestRenderMarkdownDocument`.
- **New key binding**: add a `keyType` and a decode case in `input.go`, then a
  case in `UI.applyKey`. Ctrl+R (`0x12`) is the reverse search example: it maps
  to `keyReverseSearch`, which calls `openSearchLocked`; while the overlay is
  open its own `search.key` consumes keys first.
- **A new highlighted language**: nothing to do, `chroma` resolves the fence's
  info string through its own lexer registry and aliases.
- **Third render mode**: implement `renderer` and add it to `newRenderer` and
  `ResolveMode`. The interface is deliberately small: `start`, `commit`,
  `setLive`, `resize`, `scroll`, `suspend`, `resume`, `close`, `size`.

## Known limits

- Inline mode inherits the emulator's reflow behaviour. Where that is poor, alt
  mode is the answer, and auto-detection already routes multiplexers there.
- Inline reflows whatever it hands to the emulator, which is every text line:
  prose, code, lists, quotes, diffs and tool output all go out as single logical
  lines and come back in full form when widened. Only tables and rules keep the
  width they were laid out at until they scroll away — inline never rewrites a
  committed row. This is deliberate (see *Why inline does not re-render
  committed history*): it is the price of never corrupting rows we cannot
  locate after an emulator reflow. Alt mode gives full resize fidelity: it owns
  a viewport and re-lays every line, including thematic breaks now that
  `markdown.go` retains rule intent.
- Alt mode's scrollback is ours, so the terminal scrollbar does not cover the
  session while it runs. The transcript is replayed on exit to compensate.
- Alt mode retains the whole session uncapped and re-lays every line on a width
  change, bounded by the resize debounce, not by session length. It has to keep
  everything: `close` replays the transcript onto the main screen. Inline
  retains nothing beyond its live block (committed history belongs to the
  terminal), so inline memory stays flat regardless of session length.
- A racing draw (bytes composed before a reflow and consumed after it) strands
  a row no later erase can repair — by then it is committed terminal content.
  The resize machinery exists to keep draws out of that window rather than to
  clean up behind it: no draws mid-burst, and the settled redraw gated on the
  terminal's status reply plus a quiet grace, and the per-frame generation
  abort that skips a write whose grid moved mid-compose (rules 4 and 6). What cannot be closed
  is signal latency itself: a resize whose SIGWINCH arrives after the grace
  check but before the frame lands. That window is milliseconds, and closing
  it entirely means owning the screen — alt mode.
