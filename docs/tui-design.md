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
ctx 34% ▓▓▓░░░░░░░ 68.2k/200k · opus-5
```

- Prompt is `❯ ` on the first row, two spaces on continuations.
- An empty buffer shows a dim `type a message` hint.
- The status line is one dim row: context percentage, a ten cell bar, used/total
  tokens, then the model. It escalates colour at 70% and again at 90%, and is
  truncated to the terminal width.
- A running tool adds one transient spinner row directly above the input. It is
  never committed to history; only its header and result are.
- The input block grows with the buffer, capped at a third of the screen
  (`maxInputRatio`), after which it scrolls internally around the caret.

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
  status.go        status line model and formatting, keyed segments
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

The demo driving this lives outside the package, in `main.go` and `demo.go`. It
is a scripted stand-in for the agent loop and exercises every rendering path.

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
| ctx 34% ...      follows |        | ctx 34% ...       pinned |
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

Anything above the TUI can ask the user a question: `Select`, `Confirm`, `Input`
and `Pick`, each with a `Context` variant, all blocking and all callable from a
goroutine that is not the input goroutine.

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
on whichever arrived first, and `Close` resolves everything outstanding with
`ErrCancelled` so no caller is left blocked (invariant 5).

Plain mode has no live block and `readLines` already owns stdin, so a prompt is
written to history and the answer is taken from the message queue. Reading the
same queue the caller reads is what makes it race free: a plain mode prompt is
only ever opened from the message loop, so nothing else is reading while it
waits. Registering a side channel instead loses the race whenever input is piped
rather than typed.

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
- **`flowClip`**: structural, cannot survive being broken at all. Never wrapped,
  clipped to the width instead. Tables and thematic rules.

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
| GFM table | `go-pretty`, hard sized to the terminal, clipped on resize |
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
`[spinner row?] + input rows + status row` and hands it to `renderer.setLive`
with the caret position. Anything that changes the input, status or tool state
calls `repaint`.

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

Enter submits, Alt+Enter and Ctrl+J insert a newline. Ctrl+C clears a non-empty
buffer and quits an empty one; Ctrl+D quits an empty one.

## The demo

`main.go` plus `demo.go` are a scripted stand-in for the agent loop. There is no
model behind it: `main.go` starts the UI and, on any message, plays a canned
turn. It exists to exercise every rendering path, and it drives the UI through
exactly the API the real agent loop will use, so it doubles as the reference for
how to call this package.

The first turn plays, in order:

1. **Thinking**, streamed in word chunks, dim and italic.
2. **Tool call** with a spinner, resolving to a committed result line.
3. **Markdown reply** covering headings, bold, italic, inline code, a fenced code
   block, a GFM table, a blockquote, a `----` rule, bulleted and ordered lists, a
   link and strikethrough, plus emoji and wide script samples.
4. **File edit diff** with per line and intraline highlighting.
5. **Bulk tool output**: ~170 short `go test -v` lines, deliberately narrow so
   nothing wraps, which pushes the session into scrollback and exercises
   scrolling under sustained output.
6. **Wrap up** paragraph.

Context usage in the status line ticks up across the whole turn. Later messages
play a shorter reply so the input stays live.

Two helpers in `demo.go` are worth knowing. `unwrap` joins source wrapped lines
within a paragraph into one long line, so demo prose reflows instead of being
frozen at whatever width the Go source happened to use. `expandTicks` turns `@@@`
and `@@` into fences and code spans, since Go raw strings cannot contain
backticks.

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
  emulator, with an `io.Pipe` for keystrokes.
- No `time.Sleep` in tests. Use `require.Eventually` against a locked read of the
  emulator.
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
  case in `UI.applyKey`.
- **Syntax highlighting**: fenced code blocks are styled but not highlighted.
  `chroma` would slot into `mdRenderer.codeBlock`, at roughly 5 MB of binary.
- **Third render mode**: implement `renderer` and add it to `newRenderer` and
  `ResolveMode`. The interface is deliberately small: `start`, `commit`,
  `setLive`, `resize`, `scroll`, `suspend`, `resume`, `close`, `size`.

## Known limits

- Inline mode inherits the emulator's reflow behaviour. Where that is poor, alt
  mode is the answer, and auto-detection already routes multiplexers there.
- `flowWrap` and `flowClip` lines (tables, code, diffs) do not reflow on resize in
  either mode. They keep the width they were committed at; alt mode clips the
  structural ones rather than breaking their borders.
- Alt mode's scrollback is ours, so the terminal scrollbar does not cover the
  session while it runs. The transcript is replayed on exit to compensate.
- Alt mode retains the whole session uncapped, and re-lays every line on a width
  change. Bounded by the resize debounce, not by session length.
