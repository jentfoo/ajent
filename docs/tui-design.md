# TUI Design

How `pkg/tui` works, why it is shaped this way, and the rules you must not break
when changing it.

## What it is

A terminal front end for a coding agent: a scrolling transcript of agent output
with a live input field and status line beneath it. No external TUI framework.
Dependencies are `x/term` (raw mode, size), `rivo/uniseg` (grapheme clusters and
widths), `goldmark` (markdown parsing), `go-pretty` (tables) and `go-udiff`
(diffs).

Goals, in priority order:

1. The whole session stays in scrollback and survives a terminal resize.
2. Minimal chrome. Output gets the screen, the input block costs 2 rows.
3. Correct formatting: markdown, diffs, thinking vs reply, wide characters.

Goal 1 is the one that drove every hard decision below.

## Requirements

The originating brief, and what satisfies each item.

| Requirement | Where |
|---|---|
| Thinking output, shaded so it clearly reads as thinking | `UI.Thinking`, `Theme.Thinking` (dim + italic) |
| Markdown output | `UI.Text` -> `renderMarkdown` |
| Status line under the text field, reporting context usage | `status.go`, composed by `UI.repaint` |
| Text field accepting further user replies | `editor.go` + `input.go`, submitted over `UI.Messages()` |
| Edit actions highlight the changes | `UI.Diff` -> `RenderDiff`, per line and intraline |
| Thinking visually distinct from reply output | semantic styles in `style.go` |
| As minimal as possible, maximum room for output | 2 reserved rows, no borders or rules |
| Let the CLI wrap the text where possible | prose is emitted unwrapped in inline mode |

The last one is a preference rather than an absolute, and it is honoured only for
prose. See "Wrapping policy" for what is wrapped by us and why.

## Layout and chrome

Deliberately bare. No box, no separator rule, no borders anywhere except inside
tables.

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
- Activity rows (`SetActivity`) put keyed, single-line status for work that is
  not the current tool call — their real consumer is phase 11's live sub-agent
  jobs (one row per running investigation showing its task or most recent output)
  — between any
  overlays and the input. Rendered in insertion order, each elided to width —
  never wrapped, so a row always occupies exactly one terminal line. The true cap
  is `maxActivityRows = 3` text rows **plus** a dim `+N more` indicator (see
  `activity.go`); the phase-13 doc's "four rows" was wrong and this is the
  correction. Activity is live-block only: it yields first on a short terminal
  and never reaches committed history.
- The input block grows with the buffer, capped at a third of the screen
  (`maxInputRatio`), after which it scrolls internally around the caret.

## Status line

The status block (`status.go`) is the fixed chrome beneath the input: a ten cell
context bar, used/total tokens, the model, then keyed `Segment`s in insertion
order. The bar fills against the compaction budget (`window - reserve`) so a full
bar means "compaction fires now" rather than at raw capacity; the count shows
used against the real window. A `~` prefixes the count while it is an estimate
(mid-stream or between provider reports). The colour escalates at 70% and again
at 90%, both relative to the budget.

A segment carries a `Short` form (fallback: its full text) and a `Priority`, so
a narrow terminal shortens rather than vanishes. Packing (`Status.rows`) is:

1. Everything on one row at full text, as long as it fits.
2. Otherwise the fixed part (spinner, tool, bar/tokens, model) stays on row one
   and segments move to a second row in their `Short` form.
3. Only if even two rows overflow do segments drop — lowest `Priority` first,
   ties dropping the later insertion (matching the old drop-last behaviour).

The block is capped at two rows; an overflowing segment line is clipped to width
rather than wrapped, so row accounting stays exact.

`SetStatusSegment(seg Segment)` is the single setter: add by a new key, replace
by key, remove with an empty `Text`. Because the live block is recomposed on
every repaint, a second row appearing and disappearing costs nothing structurally.
The front end publishes a `permissions` segment (`Key: "permissions"`) whenever
the live mode differs from the `allow-read` default — mirroring the reasoning
indicator. The non-default modes must always be visible so nobody forgets the gate
is open; it carries a short form (e.g. `all`, `block`, `auto`) for narrow rows.

The sub-agent manager publishes a `subagents` segment (`Key: "subagents"`) on
every transition — full form `subagents: 2 running (oldest 41s), 1 done`, short
form `sub 2` when only the count matters, cleared with an empty text when no jobs
exist. It carries a default priority and drops before `permissions` under narrow
widths, since permissions is a safety indicator that must stay visible.

## Layers

```
ui.go            public API, state machine, key handling, locking
  renderer.go        mode selection, terminal ownership, renderer interface
    render_inline.go   main screen, terminal owns wrapping and scrollback
    render_alt.go      alternate screen, we own wrapping and scrollback
  markdown.go      goldmark AST -> ANSI (+ go-pretty tables)
  diff.go          go-udiff -> colorized unified diff with intraline emphasis
  wrap.go          width aware wrapping, hanging indents
  editor.go        multi line input buffer, grapheme aware, plus its layout
  input.go         byte stream -> key events (escape sequences, paste)
  decision.go      approval dialog: context elision, numbered options, handle
  question.go      agent-initiated Ask: free-text or offered-option answer row
  activity.go      transient keyed rows above the input, capped with +N more
  status.go        status block model, two-row packing, keyed segments
  style.go         color profile detection and the palette
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

So there are two modes, chosen by `ResolveMode`:

| | Inline | Alt |
|---|---|---|
| Screen | main | alternate (`?1049h`) |
| Who wraps prose | terminal | us |
| Who scrolls | terminal | us |
| Scrollback | native, whole session | ours, replayed on exit |
| Reflow on resize | whatever the emulator does | always correct |
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

## Invariants

These are load bearing. Each one exists because breaking it produced a real bug.

**1. Inline never addresses history.** No scroll region, no absolute cursor
positioning, no repainting of committed lines. Every cursor move in
`render_inline.go` is relative (`cursorUp`, `\r`, `eraseBelow`) and confined to
the live block. This is what makes the terminal's own reflow and scrollback work
exactly as they do for `cat`. A single `cursorTo` into the history area
reintroduces the entire class of corruption we removed.

**2. Row accounting is exact, never predicted.** We emit N rows and the cursor
advances N rows. We do not compute "how many rows will the terminal wrap this
into". Predicting requires agreeing with the terminal about width, and any
disagreement (a resize mid-write, an emoji whose width the emulator measures
differently) desyncs permanently and every later write lands on live text.

**3. Committed output is never re-rendered.** Streaming markdown commits at
block boundaries only (`splitCompleteBlocks`); an open block stays buffered
until it closes. Re-rendering committed scrollback is the Ink/Claude-Code bug
that destroys history in tmux and VS Code.

The live preview must be refreshed **before** those blocks are committed: a
completed block still sitting in `r.live` would otherwise be redrawn by
`renderer.commit`'s stale-block pass as a ghost *below* the new history. When
the screen is full that oversized ghost overflows and prematurely scrolls the
just-committed lines into terminal scrollback before they are read — output
visibly jumps during streaming. So `Text`/`EndText` call `repaint()` (dropping
the completed content from the preview) ahead of `writeMarkdown`, never after.

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

Concurrency follows invariant 4. A blocking `Select` cannot hold `u.mu`, so the
caller registers a `pending` under the lock, requests a repaint, releases, then
waits on a result channel. The input goroutine checks for an active interaction
before the editor sees a key. Resolution commits a one line summary to history,
promotes the queue head and repaints. Interactions **queue in arrival order**
rather than being refused, because parallel tool calls will each want to ask
something and denying them for being simultaneous is the wrong default.
`pending.resolve` is a `sync.Once`, so a cancellation racing a keystroke settles
on whichever arrived first — it reports whether this call won, and only the
winner commits its summary or dequeues (see `routeKey`) — and `Close` resolves
everything outstanding with `ErrCancelled` so no caller is left blocked
(invariant 5). When more than one prompt waits, a dim `+N waiting` line sits
below the active interaction, reserving its row from that interaction's height
budget (caret position unchanged because it rides below).

An **approval dialog** (`OpenDecision`) is an interaction with a caller-held
handle: `Wait` blocks for the answer, `Resolve(index)` settles it from the
caller, and `Close` abandons it so `defer d.Close()` is always safe. The first
to resolve wins — whether that is a keystroke or an external resolver (phase 12's
classifier or a mode cycle) — by writing the result into `decisionState` under
`u.mu` before calling `resolve`, then committing only if it won; the loser reads
nothing. The subject is shown above numbered options, elided to at most
eight lines and 240 characters with a dim `… +N lines` marker when cut, and is
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

The question's prompt rides above the answer row in the live block and elides to
the interaction height budget with an ellipsis marker when it would overflow, so
a long multi-line question cannot blow past the bounded live block. The free-text
placeholder (`answer…`) marks where the reply is typed.

A lone `Esc` is indistinguishable from the start of a longer escape sequence
until more bytes arrive or enough time passes, so `inputReader.run` holds it for
`escTimeout` before reporting `keyEscape`. Without that there is no cancel key
at all.

## Wrapping policy

Every committed line carries a `lineFlow`, set by whoever produced it rather than
inferred from its text:

- **`flowReflow`**: prose. Emitted as one long logical line. In inline mode the
  terminal wraps and reflows it; in alt mode `wrapLine` handles it with a zero
  hang. Headings, paragraphs and thinking output.
- **`flowWrap`**: carries alignment. Wrapped by us via `wrapLine`, which breaks on
  word boundaries and indents continuations to align under the text. Code blocks,
  diffs, list items, blockquotes, user echo and raw tool output.
- **`flowReflow`** lines carrying a structured table payload (see `histLine.table`)
  are laid out fresh by `rows(width)` on every resize rather than clipped.
- **`flowClip`**: structural, cannot survive being broken at all. Never wrapped,
  clipped to the width instead. Thematic rules and tables nested inside a list or
  quote.

The trade: wrapped and clipped lines keep their alignment but do not reflow when
the terminal widens. Prose reflows but its continuations cannot be indented,
because indenting requires us to choose the break point.

The flow travels with the line because guessing it from the text was wrong:
prose legitimately starting with `-`, `+`, `@` or a box drawing rune was
misclassified and silently stopped reflowing.

`wrapLine` operates on `cell` values (grapheme cluster + active SGR state), so it
never splits a cluster, never miscounts an escape sequence as width, and reopens
the active style on each continuation row. This is why emoji with modifiers and
ZWJ sequences survive wrapping intact.
Table cells reuse the same `cell` machinery (`wrapCellLine`) so wrapped cell text
keeps its styling; column widths come from content and are shrunk (widest first)
when they would overflow, never dropping data. Rows get a separator line between
them.

## Content rendering

### Markdown

`goldmark` parses (with the GFM extension); we walk the AST ourselves in
`markdown.go` rather than registering a `NodeRenderer`, which gives control over
indentation and lets table nodes be collected and handed to `go-pretty`.

Glamour was rejected deliberately: it pulls goldmark + chroma + lipgloss for
about 20-30 modules, and it hard wraps to a fixed width, which fights the
requirement that the terminal reflow prose.

Supported elements:

| Block | Rendering |
|---|---|
| Heading | dim `#` markers kept, text in heading style |
| Paragraph | unwrapped, soft breaks become spaces so it reflows |
| Fenced / indented code | two space indent, dim language label, code style |
| Blockquote | `▏ ` prefix, dim italic body |
| List | `• ` bullets or `N. ` ordered, nested by indent, tight and loose |
| Task list | `[x]` / `[ ]` checkboxes |
| Thematic break | full width rule at the committed width, clipped on resize |
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

Fenced code is styled but not syntax highlighted. See "Extending".

### Diffs

`go-udiff` computes the change; `diff.go` renders a header (`path +N -M`), then
green additions, red deletions, cyan hunk markers and dim context. Paired
deletion/addition runs additionally get **intraline** emphasis: a character level
diff marks the changed spans in reverse video, applied only when the two lines
are at least 50% similar so unrelated rewrites are not turned into confetti.
Adjacent spans within a few characters are merged to avoid speckling.

### Semantic styling

Meaning is carried by style, not by prefix characters, so it survives wrapping.
`style.go` defines the roles; `Theme` is resolved once from the detected colour
profile (truecolor / 256 / 16 / none, honouring `NO_COLOR` and `TERM=dumb`).

| Role | Look | Used for |
|---|---|---|
| `Thinking` | dim + italic | reasoning, so it never reads as reply text |
| `User` | bold accent | echoed user messages |
| `Dim` | dim | tool output, status, context lines |
| `Accent` | magenta | markers: `✻` thinking, `⏺` tool, list bullets |
| `Heading`, `Bold`, `Italic`, `Strike`, `Code`, `Link`, `Quote` | markdown inline roles |
| `DiffAdd/Del/Hunk/File` + `*Word` | diff lines and intraline spans |

## Unicode

Widths and cursor movement go through `uniseg` grapheme clusters throughout, so
wide scripts, combining marks, emoji with skin tone modifiers and ZWJ sequences
are each treated as one unit of the correct width. A family emoji is one cluster
of width 2; both the precomposed and combining forms of `é` are width 1. Wrapping
never splits a cluster, and `truncateDisplay` never cuts one in half.

## Life of a message

```
agent (or demo) -> UI.Text("...delta")
  -> buffer until a markdown block closes      splitCompleteBlocks
  -> renderMarkdown: goldmark AST -> []histLine  markdown.go
  -> UI.commitHist: expand tabs, track section gaps
  -> renderer.commit([]histLine)
       inline: erase live block, write lines, redraw block below
       alt:    append to buffer, re-render the frame
```

The live block is rebuilt separately by `UI.repaint`, which composes
`notice? + streaming* + search? + completion? + activity* + (input | interaction)
+ status rows` and hands it to `renderer.setLive` with the caret position.
Anything that changes the input, status, tool or activity state calls `repaint`.
When a markdown block completes mid-stream, `Text`/`EndText` repaint **first** so
the just-committed rows leave the preview before `writeMarkdown` runs (invariant
3) — otherwise the inline renderer's commit pass would redraw them as a stale
ghost and push fresh output off screen.
Activity renders into whatever height remains after the status block and one
line of editor, so on a short terminal it yields first. Status is computed before
the activity budget so row accounting stays exact.

## Input

`input.go` turns bytes into `key` values: printable runes, control keys, arrows,
Home/End/Delete, PgUp/PgDn, Ctrl+Z and bracketed paste. It is a pure function
over a byte slice (`decodeKey`) plus a goroutine that feeds a channel, so it is
testable without a terminal.

Mouse reporting is deliberately not enabled. It would buy wheel events at the
cost of the terminal's own text selection for the whole session, so PgUp/PgDn
are the way to scroll in alt mode.

`editor.go` is the buffer. Positions are grapheme cluster indexes, not bytes or
runes, so the cursor moves over emoji and combining marks as a unit. It also owns
its own layout (`inputView`), hard wrapping the buffer into display rows and
reporting the caret's row and column within them.

The key table:

| Key | Effect |
|---|---|
| Enter | submit (accepts an open completion or search selection) |
| Alt+Enter, Ctrl+J | insert a newline |
| `↑`/`↓` | history recall / move line; in overlays select |
| Ctrl+C | clear non-empty buffer; interrupt when active; quit empty |
| Ctrl+D | EOF on an empty editor (quits) |
| Esc, twice | rewind onto an earlier message while idle |
| Ctrl+R | reverse history search overlay (`search.go`) |
| Shift+Tab | out-of-band `ControlModeCycle` — never consumed by the editor or a dialog; the front end cycles the permission mode |

Keys that resolve to nothing are emitted on `Controls()` as `Control` events so
the host decides their meaning. Shift+Tab is special: it reaches the control
channel even while an interaction or overlay owns the keyboard, because changing
a permission mode with a prompt already on screen must work. The front end maps
it to `Barrier.Cycle()`, which re-evaluates any open approval dialog under the new
mode — moving to `allow-all` resolves one as allow without a keystroke.

**Ctrl+R opens a reverse history search** over the workspace's recorded prompts,
drawn as an inline `(reverse-i-search)` overlay above the editor (not a modal
picker). It opens blank — nothing is shown until you type content to match against,
then typing narrows on a case-insensitive substring. Repeated Ctrl+R steps to the
next older match, Enter fills the editor with the full prompt and does not send it,
Esc closes leaving whatever was typed untouched. In the overlay ↑/↓ select: one
press fills the editor with the highlighted prompt and closes the overlay without
sending; subsequent plain arrows keep scrolling that same recorded list.

The recorded-prompt list is shared: plain ↑/↓ (no Ctrl+R) walk the same newest-first
set as the search overlay — first ↑ recalls your most recent sent message, further
↑ steps older and ↓ returns to the live draft — with a fallback to the editor's
file-history navigation when no prompt source is configured. Like completion it is
an in-place live-block overlay rather than an interaction:
the provider (`SetHistorySearch`) runs off the key loop so a slow scan never
blocks input. The overlay's pure logic lives in `search.go` with no dependency on
the UI, and the two overlays are mutually exclusive by construction.

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
VT emulator: a rune grid, cursor, scroll region, and correct **deferred wrap**
semantics (a rune written at the right margin does not wrap until the next
printable byte). Renderer tests write into it and assert on the rendered screen
rather than on raw escape bytes, which is what makes layout bugs visible.

Patterns:

- Renderer tests construct a `termState` directly with `fd: -1` and a fixed size.
  Set `sizeFn` to simulate a resize; leaving it nil pins the size.
- `newTestUI` in `ui_test.go` drives a full `UI` in inline mode against the
  emulator, with an `io.Pipe` for keystrokes. Assertions use
  `require.Eventually` against a locked read of the emulator.
- For end to end checks against a real pty, run the binary under
  `script -qec '...' /dev/null`, then resize it from outside with
  `stty -F /dev/pts/N cols 120 rows 30`. Useful for asserting what we emit;
  it cannot verify emulator reflow, since nothing is interpreting the stream.

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
- **A gutter prefix does not survive terminal wrapping.** Prose is marked by SGR
  styling only (thinking is dim + italic), never by a per-line prefix, because
  continuation rows would lose it. `flowWrap` lines can use prefixes since we
  control their breaks.

## Extending

- **New output kind**: add a method on `UI` that formats, then calls `commit`
  with the `lineFlow` its layout needs. No renderer change needed.
- **New markdown element**: add a case in `mdRenderer.block` or `inline`
  (`markdown.go`), then extend `TestRenderMarkdownDocument`.
- **New key binding**: add a `keyType` and a decode case in `input.go`, then a
  case in `UI.applyKey`. Ctrl+R (`0x12`) is the reverse search example: it maps
  to `keyReverseSearch`, which calls `openSearchLocked`; while the overlay is
  open its own `search.key` consumes keys first.
- **Syntax highlighting**: fenced code blocks are styled but not highlighted.
  `chroma` would slot into `mdRenderer.codeBlock`, at roughly 5 MB of binary.
- **Third render mode**: implement `renderer` and add it to `newRenderer` and
  `ResolveMode`. The interface is deliberately small: `start`, `commit`,
  `setLive`, `resize`, `scroll`, `suspend`, `resume`, `close`, `size`.

## Known limits

- Inline mode inherits the emulator's reflow behaviour. Where that is poor, alt
  mode is the answer, and auto-detection already routes multiplexers there.
- `flowWrap` and `flowClip` lines (code, diffs, thematic rules) do not reflow on
  resize in either mode; they keep the width they were committed at. Tables are the
exception: their structure is retained so alt mode re-lays them out at each new
width instead of clipping.
- Alt mode's scrollback is ours, so the terminal scrollbar does not cover the
  session while it runs. The transcript is replayed on exit to compensate.
- Alt mode retains the whole session uncapped, and re-lays every line on a width
  change. Bounded by the resize debounce, not by session length.
