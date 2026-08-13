//go:build demo

package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tui"
)

// drive runs the scripted demo instead of a real agent. The TUI is driven through
// exactly the API the live loop uses: on each submitted message it plays a canned
// turn, so this doubles as the reference for how to call into the package.
func drive(ui *tui.UI, set *config.Set, reg *llm.Registry, active llm.Model, sessMode resumeMode, resumeID string, args []string) *sessRec {
	// The demo has no agent or staged command to interrupt/cancel, so a quit
	// gesture (Ctrl+C or Ctrl+D on an idle empty editor) ends the show right away.
	go func() {
		for c := range ui.Controls() {
			switch c {
			case tui.ControlInterrupt, tui.ControlEOF:
				ui.Close()
				os.Exit(0)
			}
		}
	}()

	d := newDemo(ui)
	if len(args) > 0 {
		d.play(strings.Join(args, " "))
	}
	for msg := range ui.Messages() {
		d.play(msg)
	}
	return nil // the demo never records a session; nothing to resume
}

// Scripted stand in for a real agent, the TUI is driven exactly as the agent
// loop will drive it once there is a model behind it.
const (
	demoMaxTokens = 200_000 // placeholder window when no model is configured
	startTokens   = 12_400

	chunkDelay = 22 * time.Millisecond
	lineDelay  = 12 * time.Millisecond
	beatDelay  = 320 * time.Millisecond
	toolDelay  = 1400 * time.Millisecond

	// generated output stays under this width so nothing wraps at 80 columns
	maxOutputWidth = 72
)

type demo struct {
	ui     *tui.UI
	tokens int
	turn   int
}

func newDemo(ui *tui.UI) *demo {
	d := &demo{ui: ui, tokens: startTokens}
	d.setTokens(startTokens)
	return d
}

// play runs a scripted turn, the first message gets the full tour.
func (d *demo) play(string) {
	d.turn++
	if d.turn == 1 {
		d.firstTurn()
	} else {
		d.followUp()
	}
}

func (d *demo) firstTurn() {
	d.stream(d.ui.Thinking, demoThinking)
	d.ui.EndThinking()
	d.addTokens(1_900)
	time.Sleep(beatDelay)

	done := d.ui.ToolStart("bash: go test ./pkg/client/...")
	time.Sleep(toolDelay)
	done("ok  \tgithub.com/jentfoo/ajent/pkg/client\t0.412s")
	d.addTokens(3_400)
	time.Sleep(beatDelay)

	d.stream(d.ui.Text, demoReply)
	d.ui.EndText()
	d.addTokens(12_800)

	d.ui.Diff("pkg/client/retry.go", retryBefore, retryAfter)
	d.addTokens(19_400)
	time.Sleep(beatDelay)

	d.verboseRun()
	d.addTokens(18_300)

	d.stream(d.ui.Text, demoWrapUp)
	d.ui.EndText()
}

// verboseRun streams a long run of short tool output lines, none wide enough to
// wrap, so the history scrolls into the terminal's own scrollback.
func (d *demo) verboseRun() {
	lines := verboseTestLines()
	done := d.ui.ToolStart("bash: go test ./... -v")
	for _, line := range lines {
		d.ui.Output(line + "\n")
		time.Sleep(lineDelay)
	}
	d.ui.EndOutput()
	done("")
}

var demoTestPackages = []struct {
	path  string
	tests []string
}{
	{"pkg/client", []string{"TestRetry", "TestBackoff", "TestDo", "TestNewClient"}},
	{"pkg/tui", []string{"TestRenderMarkdown", "TestRenderDiff", "TestEditor", "TestDecodeKey"}},
	{"pkg/agent", []string{"TestLoop", "TestToolCall", "TestCompaction", "TestSession"}},
	{"pkg/session", []string{"TestAppend", "TestLoad", "TestPrune", "TestRotate"}},
	{"pkg/unicode", []string{"TestGraphemes", "TestWideRunes", "TestEmoji", "TestCombining"}},
}

var demoSubtests = []string{"happy_path", "context_cancelled", "invalid_input"}

// verboseTestLines returns the scripted `go test -v` transcript, every line
// truncated to maxOutputWidth columns so the output never wraps.
func verboseTestLines() []string {
	out := make([]string, 0, 160)
	add := func(format string, args ...any) {
		out = append(out, tui.TruncateDisplay(fmt.Sprintf(format, args...), maxOutputWidth))
	}
	for p, pkg := range demoTestPackages {
		for i, name := range pkg.tests {
			add("=== RUN   %s", name)
			for j, sub := range demoSubtests {
				add("=== RUN   %s/%s", name, sub)
				add("    --- PASS: %s/%s (0.0%ds)", name, sub, j)
			}
			add("--- PASS: %s (0.%02ds)", name, i+2)
		}
		add("PASS")
		add("ok  \tgithub.com/jentfoo/ajent/%s\t0.%03ds", pkg.path, 214+p*97)
	}
	return out
}

func (d *demo) followUp() {
	d.stream(d.ui.Thinking, "Checking what is already in place before answering.\n")
	d.ui.EndThinking()
	time.Sleep(beatDelay)

	d.stream(d.ui.Text, demoFollowUp)
	d.ui.EndText()
	d.addTokens(4_300)
}

// stream feeds text to sink a few words at a time to imitate token streaming.
func (d *demo) stream(sink func(string), text string) {
	for _, chunk := range chunks(text) {
		sink(chunk)
		time.Sleep(chunkDelay)
	}
}

// chunks splits text on whitespace, keeping the separators attached.
func chunks(text string) []string {
	var out []string
	var start int
	for i, r := range text {
		if r == ' ' || r == '\n' {
			out = append(out, text[start:i+1])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}

func (d *demo) addTokens(n int) { d.setTokens(d.tokens + n) }

func (d *demo) setTokens(n int) {
	d.tokens = n
	d.ui.SetTokens(n) // the model and its window belong to the registry, not the demo
}

// unwrap joins the source wrapped lines of each paragraph into a single long
// line, so the terminal decides where to break rather than this file. Markdown
// gets this for free through soft line breaks, plain text such as thinking does not.
func unwrap(s string) string {
	paragraphs := strings.Split(strings.TrimSuffix(s, "\n"), "\n\n")
	for i, p := range paragraphs {
		paragraphs[i] = strings.Join(strings.Split(p, "\n"), " ")
	}
	return strings.Join(paragraphs, "\n\n") + "\n"
}

var demoThinking = unwrap(`The retry helper in pkg/client/retry.go loops a fixed number of times with no
backoff and no way to cancel, so a hung call blocks the whole agent loop.

Plan: thread a context through the call, add exponential backoff between
attempts, then cover both with a table driven test. The cap needs its own case
since an unbounded shift overflows the duration after about twenty attempts.
`)

// expandTicks turns @@@ into a code fence and @@ into an inline code span, since
// Go raw strings cannot contain backticks
func expandTicks(s string) string {
	s = strings.ReplaceAll(s, "@@@", "```")
	return strings.ReplaceAll(s, "@@", "`")
}

//nolint:gosmopolitan // wide script samples are deliberate, they exercise the width math
var demoReply = expandTicks(`## Retry hardening

I threaded a **context** through @@retry@@ and added exponential backoff, so a
hung call can no longer block the whole agent loop.

### What changed

- ✅ **Cancellation** so the loop returns as soon as the context is done
- ⏱️ **Backoff** where @@backoff(i)@@ grows 50ms, 100ms, 200ms, capped at 2s
- 🧪 **Tests** with one table driven case per failure mode

The cap lives inside @@backoff@@ itself, so every caller gets it for free:

@@@go
func backoff(attempt int) time.Duration {
	if attempt > maxShift {
		return maxDelay
	}
	return min(baseDelay<<attempt, maxDelay)
}
@@@

| Case | Attempts | Result |
|---|---:|---|
| first call succeeds | 1 | ✅ nil |
| transient failure | 3 | ✅ nil |
| context cancelled | 1 | ❌ context.Canceled |
| 日本語のケース | 2 | ✅ nil |

> The cap matters more than the base delay here, an unbounded shift overflows
> the duration after about twenty attempts.

----

### Follow ups

1. Thread the same context through @@client.Do@@
2. Replace the *fixed* attempt count with a deadline
3. See [the retry notes](https://ajent.dev/retry) for the rationale

Nothing here changes the public API, so ~~a major version bump~~ a patch
release is enough. 🚀

Unicode widths are handled by grapheme cluster, so wide scripts (日本語, 中文,
한국어), combining marks (é vs é), emoji with modifiers (👍🏽, 👨‍👩‍👧‍👦) and
symbols (→ ✓ ∑ ≈ °C) all measure and wrap correctly.
`)

var demoWrapUp = unwrap(`Full suite is green across all five packages ✅. The overflow case at attempt 40
covers the cap, so the shift can no longer wrap around. Try resizing the window,
the prose above should re-wrap while tables, diffs and code keep their shape.
`)

var demoFollowUp = expandTicks(`Already handled. The cap is applied inside @@backoff@@ itself, so every caller
gets it for free, and the test covers the **overflow case** at attempt 40.
`)

const retryBefore = `package client

func retry(n int) error {
	for i := 0; i < n; i++ {
		if err := call(); err == nil {
			return nil
		}
	}
	return errFailed
}
`

const retryAfter = `package client

func retry(ctx context.Context, n int) error {
	for i := 0; i < n; i++ {
		if err := call(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff(i)):
		}
	}
	return errFailed
}
`
