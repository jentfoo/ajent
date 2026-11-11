# TUI

The interactive terminal surface: paint modes, what stays permanent versus
what redraws, every key combination on every surface (prompt, overlays,
dialogs, rewind picker, attach), the status line, markdown rendering and
terminal images. The design philosophy behind it (README's *Design Philosophy*
section) is copy-and-paste-friendly output over chrome: a permanent transcript
above the prompt bar, a minimal dynamic lower section. The input-line grammar
(`/command`, `!shell`, `!!`, `@path`) lives in `commands.md`, permission modes
and what each dialog answer grants in `permissions.md`, rewind/fork session
effects in `sessions.md`, front-end selection and attach setup in `cli.md`,
config layers and `AJENT_*` bindings in `config.md`.

## Paint modes

| Mode | Screen | Who wraps prose | Scrollback |
|---|---|---|---|
| `inline` (auto default) | the main screen | the terminal | native, the whole session survives resize and scrolling |
| `alt` | its own alternate screen | ajent | ajent's buffer, replayed onto the main screen at exit so nothing is lost |
| `plain` | no escapes at all | the terminal | none drawn |

`auto` resolves in this order: not a terminal or `TERM=dumb` picks `plain`,
tmux/screen (`$TMUX`, `$STY`, a matching `TERM`) picks `alt` because
multiplexers reflow poorly, everything else picks `inline`. An explicit value
(`--render`, `ui.render`) overrides detection.

- **Resize**: inline leaves committed lines to the emulator's own reflow and
  redraws only the live block. Alt re-lays everything from retained lines, so
  tables and rules always fit the new width.
- **Selection**: inline copies with the terminal's native selection, alt only
  what is on screen.

## Screen anatomy

```
<committed transcript, permanent>

❯ your message
  second line of a multi-line draft
▓▓░░░░░░░░ ~12k/200k · anthropic/claude-opus-4 · allow-read
```

Everything above the prompt rule is permanent history and never changes. The
lower section is dynamic: one dim `─` divider, then (top to bottom) a keyed
notice, streaming thinking and reply previews, an open search or completion
overlay, activity rows, queued prompts, the input field (or a dialog in its
place), and the status line. The whole chrome costs three rows plus whatever
the dynamic content needs.

- **Prompt**: `❯` on the first row, continuation lines indented two spaces.
  An empty buffer shows the dim hint `type a message`. A draft starting with
  `!` drops the glyph so the shell marker reads clean (`commands.md`).
- **Activity rows**: live work between overlays and the input, full-width
  shaded bands. Two producers: one row per running sub-agent job (task or most
  recent output, oldest first) and tool-call progress while a call streams in
  (name, target, bytes so far). At most four text rows, then one dim `+N more`
  line.
- **Queued prompts**: steering messages sent mid-turn wait as dim rows above
  the input until the turn boundary. Alt+↑ pulls the newest back for editing.
  On a short terminal these yield first.

## Scrollback and copying

Copying content out is why the layout stays bare:

- Prose commits as one logical line per paragraph, so the terminal wraps it,
  reflows it on resize, and a selection copies clean text with no trailing
  padding, fake indents or wrapped-continuation noise. Tabs expand to four
  spaces at commit.
- Committed output is never rewritten in inline mode (the corruption risk that
  destroys tmux and VS Code history is why), so what you scroll up to is what
  was printed.
- Mouse reporting is deliberately off, which keeps the terminal's own text
  selection working for the whole session.
- `/copy` puts the last agent reply on your clipboard and `ctrl+x` copies the
  highlighted message in the rewind picker (`commands.md`). Both work through
  OS backends and fall back to OSC 52 over SSH, naming what to install when
  nothing applies.
- A rewind or resume never erases scrollback: a solid divider band marks where
  restored context begins and the branch replays below it, so scrolling up
  still reads continuously.

## Status line

One dim row (two on narrow screens) beneath the input, in fixed order:
working spinner glyph · context bar with used/total tokens · model · segments.

- **Context bar**: a ten-cell block filling against where auto-compaction
  fires when it is enabled, otherwise the window minus the response reserve,
  so a full bar means "compaction now", not raw capacity. The count shows used
  against the real window and carries `~` while streaming (estimated). Color
  escalates near the budget: dim below ~70%, the code hue to ~90%, red past.
- **Spinner**: animates whenever work runs, colored by what it waits on. A
  running tool takes the newest named call's color, streamed output another,
  plain waiting a third. At rest it holds one static frame.

Segments follow in declared order regardless of which publisher spoke first:

| Segment | Shows |
|---|---|
| reasoning | active level when it differs from the configured default |
| mcp | `mcp: active/discovered` registered tool count |
| subagents | `subagents: 2 running (oldest 30s), 1 queued, 3 done`, short `sub N` |
| bash bg | backgrounded shell commands (`bash bg 1`) |
| plan | `/plan` phase and round, like `plan r2/4` |
| permissions | current permission mode label, kept full late in the collapse ladder so an open gate is never missed |
| hint | one transient imperative line (below), newest claim wins |

On a narrow row each piece collapses to its short form in priority order. Only
a fully collapsed single row that still overflows splits in two: fixed part and
model stay on row one, segments pack row two and drop before anything clips.

The hint segment is shared by every transient notice of the typing hold and the
control loops: `ctrl+c again to quit`, `detaching…`, `cancelled shell
command`, a queue-drop outcome. A newer claim takes the line and hands it back
to the previous holder when released, so two async producers cannot erase each
other.

## Markdown rendering

Streaming output commits at **block boundaries**: a paragraph, heading or code
fence lands in history only once complete. The open tail renders live above the
input, wrapped the same way its commit will lay it out, so text never visibly
re-flows as it lands. A preview longer than the screen keeps its tail and marks
the dropped head with a dim `•••`.

| Block | Rendering |
|---|---|
| Heading | dim `#` markers kept, heading style on the text |
| Paragraph | unwrapped so the terminal owns wrapping, soft breaks fold to spaces |
| Fenced code | indented, dim language label, syntax highlighted where color depth allows (256-color and up), flat code style otherwise |
| Blockquote | narrow prefix, dim italic body |
| List / task list | bullets or numbers, nested by indent, checkboxes for tasks |
| Thematic break | rule drawn to the width in force |
| GFM table | cells wrapped per column, separator row between entries, clipped rather than garbled when too wide |
| HTML | passed through dim |

Inline styles: emphasis (italic/bold), code spans, strikethrough, links styled
with a dim URL (hidden when they match the text), images named by alt text.
Nested styles stack correctly.

- **Thinking** streams after a `✻ thinking` header in dim italic and commits
  line by line. It is dropped from restored history on rewind/resume/attach,
  where it reads as noise.
- **Tool output** reaches history through a head: the first few lines commit
  verbatim, the rest folds into one dim summary row (`… +180 lines, 24kb`) at
  call end. Your own `!` and `!!` runs show everything instead
  (`commands.md`). The model always receives full output.
- **Diffs** render git-shaped: a file header with `+N -M`, one `@@` hunk per
  region with three context lines, right-aligned line numbers and gutter
  markers. Changed characters get intraline reverse-video emphasis when the
  pair is similar enough not to speckle. A proposed edit commits its diff
  before approval opens, so the dialog below it never repeats a truncated copy.

## Images

Display is detected conservatively from the environment: kitty graphics for
kitty, Ghostty and Konsole, the iTerm2 inline protocol for iTerm, WezTerm and
mintty. Multiplexers, non-terminals and unknown `TERM`s report none. An
explicit `ui.images` (`kitty`, `iterm2`, `none`) overrides detection outright.

- Images draw into inline history only, committed exactly once so they can
  never stack copies on a repaint. Alt and plain render the placeholder.
- A drawn image is clamped to the terminal width and half its height, both
  axes scaled together so the aspect survives.
- Where nothing draws (or on replay after resume) the honest placeholder reads
  `[image shot.png 640x480, 12kb]`.
- An image submitted with a prompt keeps an `[image #N]` token in the editor,
  so recalling that line restores the reference. A token whose slot is gone
  drops at submit with a notice rather than vanishing silently.
- Ctrl+V probes the clipboard and attaches a found image (see key table).
  `images.block: true` stops any image reaching the model, replacing each with
  an "Image reading is disabled." note (`config.md`).

## Keys at the prompt

| Key | Effect |
|---|---|
| typing / paste | insert at the caret. Positions are grapheme clusters: emoji and combining marks move as one unit |
| Enter | submit (the default, rebindable via `ui.keymap`) |
| Ctrl+J, Alt+Enter | insert a newline (the default newlines, rebindable) |
| Backspace | delete back one cluster |
| Delete | forward delete. Ctrl+D on an empty editor quits, otherwise deletes forward |
| ←/→ or Ctrl+B / Ctrl+F | move by cell |
| Alt+B / Alt+F, Ctrl+←/→, Alt+←/→ | move by word |
| Home / Ctrl+A, End / Ctrl+E | start/end of the current visual row (not the whole buffer) |
| ↑/↓ or Ctrl+P / Ctrl+N | cursor-first through visual rows at roughly the same column. Only a press already on the very first character recalls older history (symmetrically ↓ or Ctrl+N on the last for newer), so scrolling from an edited line takes two presses. Mid-text, one press jumps to the row's start/end |
| PgUp / PgDn | inline: page a multi-line draft toward its head/tail. Alt: scroll committed output |
| Ctrl+K | clear from the caret to the end of the visual row (content below joins at the caret) |
| Ctrl+U | clear from line start to the caret |
| Ctrl+W or Alt+Backspace | delete the word before the caret |
| Ctrl+Y | restore what the last clear or kill removed, one level deep. Any later edit drops it |
| Esc with text | clears the whole draft (Ctrl+Y restores) |
| Esc on an empty editor | interrupts a running turn, and only while idle arms the rewind gesture (below). A lone press still counts as interrupt once the window passes |
| Ctrl+C | with text: clear the draft. With nothing to stop: `ctrl+c again to quit` appears and a second press inside ten seconds quits |
| Ctrl+Z | suspend ajent, repaint on return |
| Ctrl+L | redraw the screen (the dynamic block only, committed output untouched) |
| Ctrl+R | open reverse history search (below) |
| Alt+↑ | recall the newest queued prompt into the editor for editing or resubmitting |
| Tab | completion (below) |
| Shift+Tab / Shift+→ | cycle the permission mode forward. Out-of-band: works even while a dialog owns the keyboard, and re-evaluates any open approval dialog under the new mode (`permissions.md`) |
| Shift+← | same cycle one mode back |
| Ctrl+V | probe the clipboard and attach an image as `[image #N]` (kitty's own paste binding wins where mapped in `kitty.conf`, pasting text only) |
| Ctrl+X | while a picker is open: copy the highlighted message to the clipboard. Inert everywhere else, so it never reaches the editor |

Ctrl+P / Ctrl+N behave as ↑/↓ on every surface that lists arrow keys below (dialogs,
pickers, menus, search, rewind, attach), not just at the prompt.

**Pastes**: a bracketed paste over ~1 KB collapses to a `[pasted N lines #M]`
placeholder that keeps the input block small and expands back to full text at
submit. Terminals without bracketing coalesce bulk keystrokes into one paste
the same way, while normal typing replays untouched. Entries survive for the
session, so a recalled prompt still resolves.

The `ui.keymap` setting moves submit/newline roles between the three physical
keys (`enter`, `ctrl+j`, `alt+enter`). Confirmation surfaces always accept
Enter regardless, so remapping cannot strand a dialog
(`config.md`).

### History recall and search

- Plain ↑/↓ walk **this session's own prompts**, newest first, holding your
  draft aside so ↓ brings it back. With no prompts of its own (a fresh
  session) arrows fall back to the workspace-wide set. While the list loads,
  a transient `loading history…` status segment explains any wait.
- **Ctrl+R** searches one merged workspace-wide source: every typed line, plus
  `/cmd` and `!shell` lines and recorded prompts across sessions, deduplicated.

| Key (search overlay) | Effect |
|---|---|
| typing | narrows on a case-insensitive substring, matches emphasized. Blank until you type |
| Ctrl+R again | step to the next older match |
| ↑/↓ | accept the highlighted line into the editor without sending, then browse from there |
| Enter | fill the editor with the current match, caret on the matched text. Never sends |
| ←/→ (and word-wise forms), Home | accept and apply their motion in one press so editing starts at once. With no match they only close |
| Esc | accepts a current match, otherwise closes. A second Esc then clears the field |
| Ctrl+C | close |

### Completion

- `/` at line start opens the command menu as you type (`commands.md`), `@`
  completes paths (`commands.md`), and `!`/`!!` lines complete like bash.
- Tab fills the candidates' longest common prefix, or with nothing more to
  fill opens a listing. When there is nothing to add at all the divider above
  the prompt flashes briefly instead.
- In an open menu: ↑/↓ cycle the highlight and mark the selection moved, Tab
  accepts it. Enter on a moved selection accepts-and-submits in one press,
  otherwise Enter submits the line as typed, so an open menu never swallows a
  send. Any editing key clears the moved state, Esc dismisses without
  inserting.

## Dialogs and pickers

Anything that needs an answer (permission approvals, `/model`, rewind,
questions from the agent) opens in the dynamic block in the input's place.
Your draft is kept aside and returns when it resolves. Interactions queue in
arrival order with a dim `+N waiting` row, since parallel tool calls can ask at
once. An approval dialog never steals focus mid-draft: while you type it waits
behind an `approval Ns` countdown segment and raises only once you pause,
clear or submit (`permissions.md`). In plain render mode there is no dynamic
block: the prompt prints into history and the answer comes from typed stdin
lines.

**Choice list** (Select / Confirm): ↑/↓ move with wraparound, digits 1-9 pick
directly up to nine options, Enter confirms, Esc or Ctrl+C cancels. A summary
line of the choice commits to history.

**Filterable picker** (rewind tree, `--attach`'s live-session list, `/model`):

| Key | Effect |
|---|---|
| typing / Backspace / Ctrl+U | edit the filter. Verbatim matches rank over fuzzy subsequence hits |
| ↑/↓ | move with wraparound |
| PgUp / PgDn | jump five rows |
| Enter | choose (inert while nothing matches) |
| Esc, Ctrl+C | cancel |

**Multi-select** (`/tools`): Space or Tab toggles the highlighted row, a group
header row toggles its whole group (`[x]` all, `[ ]` none, `[~]` partial),
typed text narrows (a space never joins it), Enter confirms, Esc cancels.

**Text input**: single line (newlines fold to spaces), Backspace trims one
cluster, Ctrl+U clears, Enter submits, Esc cancels. A secret input masks every
character as `•` on screen and in the committed summary, so a pasted key never
lands in scrollback.

**Approval dialog** (`permissions.md`): numbered options 1-9 answer directly,
↑/↓ move with Enter confirming, Esc or Ctrl+C returns "cancelled", which for
approval means deny. The subject above the options is the real command or
diff summary: lines wrap rather than clip (the first line always survives),
elided past sixteen lines with a `…+N lines` marker, never markdown-rendered.

**Agent question** (`agents.md`): prompt text rides above numbered options
like a choice list, plus a `Chat about this` row. Taking it swaps to free-text
editing (full prompt editing keys, Tab path completion when offered). Esc in
the reply goes back to the options. Esc on the option list declines to answer,
recorded as an ordinary result rather than an error. Ctrl+C declines from
anywhere.

## Rewind picker

`Esc Esc` while idle and not mid-turn opens the context-tree picker over every
message ever recorded (400ms window between presses, so a lone Esc still works
as interrupt). With recording disabled there is nothing to rewind onto and the
gesture says so. Rewinding is what `sessions.md` describes. On screen:

- Rows carry branch guides (`├──`) and a colored role tag: user, agent or
  tool. Rows on the active branch render saturated, abandoned forks recede to
  faint with dim bodies (with color off, a `*` gutter marks the active chain).
- The list opens at the current head, so reopening after a rewind lands where
  you were.
- `ctrl+x` copies the highlighted message verbatim from the transcript,
  independent of the collapsed label on screen.
- On large trees (300+ rows) alt mode defers repainting committed history
  while you navigate, so arrowing stays instant.
- Choosing replays that branch below a divider band. Esc cancels.

## Attach

An attached terminal mirrors a live session and shares its keyboard
(`cli.md`). Editing, recall (`Ctrl+R`, ↑/↓ over the same local stores),
completion and dialogs behave exactly as locally. The out-of-band keys route
to the remote session instead:

| Key | Effect in attach |
|---|---|
| Esc | interrupts the remote turn |
| Ctrl+C | interrupt when something runs, otherwise arms `ctrl+c again to detach`, a second press inside ten seconds leaves the mirror (the session keeps running) |
| Shift+Tab / Shift+→, Shift+← | cycle the remote permission mode |
| Alt+↑ | pop the served run's newest queued prompt into this editor for editing and resubmitting there |
| Ctrl+V | refused with a notice: clipboard images cannot ride an attach |
| Ctrl+X | copies picker rows locally as usual |

Who answers dialogs depends on the control seat (`sessions.md`): holding it,
you answer approval and question dialogs directly, racing any other surface
(first resolution wins). A demoted viewer sees each open dialog as an info
notice instead. Opening the rewind picker shows the remote tree, and a rewind
made from the other terminal closes your stale copy of it.

## Tuning

Every key here has its layer and `AJENT_*` binding in `config.md`.

- `ui.render` / `--render`: paint mode (`auto`, `inline`, `alt`, `plain`).
- `ui.color`: force the detected color depth when a terminal classifies badly;
  `256` is what turns syntax highlighting on where detection stayed
  conservative (`config.md`).
- `ui.theme`: palette, chosen by the first-run picker (`cli.md`) and
  switchable later from `/settings → Theme`. Restyling is forward-only:
  history already on screen keeps the colors it was written with.
- `ui.images`: force an image protocol (`auto`, `kitty`, `iterm2`, `none`).
- `ui.keymap`: which keys submit and which insert a newline (above).

## See also

- `commands.md` - slash commands, `/copy`, the input-line grammar (`@path`,
  `!shell`, `!!`)
- `permissions.md` - permission modes, approval dialogs, safe/denied matching
- `sessions.md` - rewind/fork semantics, compaction, resume replay
- `cli.md` - front-end selection (`--render`, `--attach`), first-run theme picker
- `config.md` - every `ui.*` key with its layer and env binding
