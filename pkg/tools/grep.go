package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
)

// output modes for grep, shared by validation and rg invocation.
const (
	grepContent = "content"
	grepFiles   = "files"
	grepCount   = "count"
)

// grepParams is the model-facing parameter block for grep.
type grepParams struct {
	Pattern    string `json:"pattern" desc:"regex, or a literal string when literal is true"`
	Path       string `json:"path,omitempty" desc:"directory to search in; default the session cwd"`
	Glob       string `json:"glob,omitempty" desc:"only search files matching this glob, e.g. '*.go'"`
	IgnoreCase bool   `json:"ignoreCase,omitempty" desc:"case-insensitive search"`
	Limit      int    `json:"limit,omitempty" desc:"max matches to return"`
	Literal    bool   `json:"literal,omitempty" desc:"treat pattern as a literal string instead of a regex"`
	Context    int    `json:"context,omitempty" desc:"lines to show before and after each match (content mode)"`
	Mode       string `json:"mode,omitempty" enum:"files,content,count" desc:"output mode; default content with line numbers"`
}

// grepTool searches files, shelling out to rg when present and falling back to
// a bounded Go regexp walk. Content output is capped so one minified file stops
// on bytes.
type grepTool struct {
	policy    PathPolicy
	sessionID string // names the spill directory for long results
	forceGo   bool   // skip rg even when present, so tests exercise the Go fallback
}

var _ agent.Tool = (*grepTool)(nil)

func (t *grepTool) Name() string { return ToolGrep }

func (t *grepTool) Label(agent.ToolCall) string {
	return ToolGrep
}

func (t *grepTool) Description() string {
	return "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore."
}
func (t *grepTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[grepParams]()} }
func (t *grepTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}
func (t *grepTool) Close() {}

// selfBounding: grep bounds and spills its own results.
func (*grepTool) selfBounding() {}

// Execute runs the search, bounded by the limit and GrepResult.
func (t *grepTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p grepParams
	if err := decode(call.Input, &p); err != nil {
		return agent.ErrorToolResult("bad args: " + err.Error()), nil
	}
	if p.Pattern == "" {
		return agent.ErrorToolResult("grep needs a non-empty pattern"), nil
	}
	cwd, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return agent.ErrorToolResult(err.Error()), nil
	}

	mode := p.Mode
	switch mode {
	case "", grepContent, grepFiles, grepCount:
	default:
		return agent.ErrorToolResult("grep mode must be files, content or count"), nil
	}

	// compile first: a bad regex must surface as an error, not empty results
	re, err := p.compile()
	if err != nil {
		return agent.ErrorToolResult("grep: " + err.Error()), nil
	}

	grepLimit := GrepResultLimit()
	max := p.Limit
	if max <= 0 {
		max = grepLimit.Lines
	}

	if !t.forceGo && rgOnPath() {
		out, cut, warn, rgErr := runRg(ctx, cwd, p, mode, max)
		if rgErr != nil {
			return agent.ErrorToolResult("grep: " + rgErr.Error()), nil
		}
		note := p.capNote(mode, cut, max)
		if warn != "" { // rg warned about paths it could not read, mirror the fallback's note
			note = joinNotes(note, "rg: "+strutil.FirstLine(warn))
		}
		// Display mirrors the model-visible text
		return t.finalize(out, note), nil
	}

	return t.goSearch(ctx, cwd, p, mode, re, max), nil
}

// capNote names a spent match budget so a stopped search is never mistaken for
// complete. The rg path sets it only on proven withholding; the fallback cannot
// tell an exact fill from more to come without scanning past the budget, so its
// conservative note stays. Count output and context blocks always name it: a trimmed
// count reads as authoritative in a way cut-off content lines do not, and a block
// cut withholds whole matches even under an explicit limit. Plain content and
// files results stay silent under an explicit limit because that cap is the
// model's own choice. The budget counts matches, never output lines, so
// context-heavy results are not falsely named when they complete naturally.
func (p grepParams) capNote(mode string, spent bool, max int) string {
	blocks := (mode == "" || mode == grepContent) && p.Context > 0
	if !spent || (p.Limit > 0 && mode != grepCount && !blocks) {
		return ""
	}
	return fmt.Sprintf("... result cap of %d matches reached; narrow the pattern or raise limit", max)
}

// joinNotes combines two notes, dropping either when empty and joining with a
// space so both can ride in one footer line.
func joinNotes(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + " " + b
	}
}

// compile builds the matcher, honouring literal and ignoreCase.
func (p grepParams) compile() (*regexp.Regexp, error) {
	pat := p.Pattern
	if p.Literal {
		pat = regexp.QuoteMeta(pat)
	}
	if p.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, fmt.Errorf("invalid pattern %q: %w", p.Pattern, err)
	}
	return re, nil
}

// maxGrepWorkers bounds the fallback grep's concurrent file reads and matches.
const maxGrepWorkers = 8

// goSearch walks cwd with the compiled matcher when rg is unavailable. Scans
// run concurrently in walk order. The assembled output matches the sequential
// walk byte for byte, budget included.
func (t *grepTool) goSearch(ctx context.Context, cwd string, p grepParams, mode string, re *regexp.Regexp, max int) agent.ToolResult {
	type fileMatch struct {
		rel  string
		hits []grepHit
		ends []int // ends[i]: exclusive hits index at match i's context block end
		n    int
		skip bool // inspected but binary or unreadable, can hold no matches
	}
	paths := bulk.SliceFilterInPlace(func(path string) bool {
		return p.Glob == "" || matchGlob(p.Glob, grepRel(cwd, path))
	}, repoFiles(ctx, cwd)) // .gitignore semantics on the fallback too

	workers := min(runtime.GOMAXPROCS(0), maxGrepWorkers)
	// one scan covers twice the worker count: enough to keep every worker busy,
	// small enough that an exhausted budget wastes at most one batch of scans
	window := 2 * workers
	sem := make(chan struct{}, workers)
	scan := func(lo, hi, budget int) []fileMatch { // scan paths[lo:hi] concurrently
		results := make([]fileMatch, hi-lo)
		var wg sync.WaitGroup
		for i := lo; i < hi; i++ {
			if ctx.Err() != nil {
				break
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			go func(i int, path string) {
				defer wg.Done()
				defer func() { <-sem }()
				var m fileMatch
				f, err := os.Open(path)
				if err != nil {
					m.skip = true // inspected but unreadable, report it as not searched
					results[i-lo] = m
					return
				}
				defer func() { _ = f.Close() }()
				head := make([]byte, sniffLen)
				n, _ := io.ReadFull(f, head) // n < len(head) when the whole file fit
				if binary(head[:n]) {
					m.skip = true // inspected but unsearchable, report it as not searched
					results[i-lo] = m
					return
				}
				var data []byte
				switch {
				case n < len(head): // whole file fit in the sniff window: no remainder to read
					data = head[:n]
				default:
					// a text file continues on the same handle. Binaries never load past the
					// sniff, so a big binary is rejected without its full bytes ever allocated.
					var buf bytes.Buffer // accumulates head + remainder into one backing array
					buf.Write(head)
					if _, rerr := io.Copy(&buf, f); rerr != nil {
						m.skip = true // inspected but unreadable, report it as not searched
						results[i-lo] = m
						return
					}
					data = buf.Bytes()
				}
				m.rel = grepRel(cwd, path)
				m.hits, m.n, m.ends = goGrep(normalizeToLF(string(data)), re, p.Context, budget)
				results[i-lo] = m
			}(i, paths[i])
		}
		wg.Wait()
		return results
	}

	var matches []string
	counts := map[string]int{}
	var skipped int  // files inspected but binary or unreadable, so not actually searched
	var printed bool // any content line emitted yet, gating the leading "--" separator
	var prevLine int // line number of the last emitted line, for block contiguity
	remaining := max
	for lo := 0; lo < len(paths) && remaining > 0 && ctx.Err() == nil; lo += window {
		budget := remaining // no file in this scan can emit more than the budget left
		for _, m := range scan(lo, min(lo+window, len(paths)), budget) {
			if m.skip {
				skipped++
				continue
			}
			if m.rel == "" {
				continue // never launched: the budget or context cut this batch short
			}
			n := m.n
			if mode != grepFiles && remaining > 0 && n > remaining {
				n = remaining // trim to the shared budget, files mode spends one per file
			}
			switch mode {
			case grepCount:
				if n > 0 {
					counts[m.rel] = n
				}
			case grepFiles:
				if m.n > 0 {
					matches = append(matches, m.rel)
					remaining--
				}
			default:
				if n < m.n {
					m.hits = m.hits[:m.ends[n-1]] // keep context through the n-th match
				}
				for i, h := range m.hits {
					// rg's text printer: "--" separates disjoint blocks and file changes,
					// but only when context lines are requested at all
					if p.Context > 0 && printed && (i == 0 || h.line != prevLine+1) {
						matches = append(matches, "--")
					}
					sep := ":" // match lines print path:line:text, context ones path-line-text
					if !h.match {
						sep = "-"
					}
					matches = append(matches, fmt.Sprintf("%s%s%d%s%s", m.rel, sep, h.line, sep, h.text))
					prevLine, printed = h.line, true
				}
			}
			if mode != grepFiles {
				remaining -= n
			}
			if remaining <= 0 {
				break
			}
		}
	}

	var b strings.Builder
	if mode == grepCount {
		paths := slices.Sorted(maps.Keys(counts))
		for _, pth := range paths {
			fmt.Fprintf(&b, "%s:%d\n", pth, counts[pth])
		}
	} else {
		for _, m := range matches {
			b.WriteString(m + "\n")
		}
	}

	trimmed := strings.TrimRight(b.String(), "\n")
	note := p.capNote(mode, remaining <= 0, max)
	if skipped > 0 { // only when something could not be searched: results may miss matches there
		n := fmt.Sprintf("%d file(s) not searched (binary/unreadable); results may be incomplete", skipped)
		note = joinNotes(note, n)
	}
	return t.finalize(trimmed, note)
}

// finalize bounds out to GrepResult, spilling the complete text when cut.
// note, when non-empty, rides after the bounded text and its footer, so the
// bound never cuts it.
func (t *grepTool) finalize(out, note string) agent.ToolResult {
	out = normalizeToLF(out) // rg and go paths both carry \r on CRLF files, LF-only to the model
	text, _ := truncateOutput(t.sessionID, ToolGrep, out, GrepResultLimit(), "")
	if note != "" {
		text += "\n" + note
	}
	return agent.ToolResult{Content: llmBlock(text), Display: text}
}

// rgOnPath reports whether ripgrep is available.
func rgOnPath() bool {
	_, err := exec.LookPath("rg")
	return err == nil
}

// runRg streams ripgrep output until the report is complete or one more item
// arrives past the match budget, then cancels rg so a deep tree with many
// matches does not keep scanning. Returns the result, whether anything was
// withheld past the budget (cut: an exact fill ends naturally and stays
// unnamed), any benign stderr warning (warn) to mirror the fallback's
// incomplete-coverage note, and any failure. Output lines are rewritten to
// root-relative paths so both engines print one shape. Exit status 1 means "no
// matches", while an early cancel ignores whatever exit code follows. No -m is
// passed: a per-file match bound would end an over-budget file at exactly max,
// dressing real withholding up as a natural end.
func runRg(ctx context.Context, root string, p grepParams, mode string, max int) (string, bool, string, error) {
	args := []string{"--no-heading", "--color=never", "-H"} // -H: one file keeps its name prefix
	switch mode {
	case grepFiles:
		args = append(args, "-l")
	case grepCount:
		args = append(args, "-c")
	default:
		args = append(args, "-n")
	}
	if p.IgnoreCase {
		args = append(args, "-i")
	}
	if p.Literal {
		args = append(args, "-F")
	}
	contentMode := mode == "" || mode == grepContent
	blockMode := contentMode && p.Context > 0 // whole blocks: matches counted as they stream
	if blockMode {
		args = append(args, "-C", strconv.Itoa(p.Context), "--json")
	}
	if p.Glob != "" {
		args = append(args, "--glob", p.Glob)
	}
	args = append(args, "--", p.Pattern, root)

	cmd := exec.CommandContext(ctx, "rg", args...)
	var stderr bytes.Buffer
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", false, "", fmt.Errorf("rg: %w", err)
	}
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", false, "", fmt.Errorf("rg: %w", err)
	}

	var b strings.Builder
	var stopped bool                         // true once rg was cancelled early; waitErr and stderr follow from it
	var cut bool                             // true only once content arrived past the budget, proving withholding
	r := bufio.NewReaderSize(stdout, 64<<10) // no line-length cap, minified lines survive

	// withheld classifies the read that follows a spent budget: any arriving line is
	// content past the cap, so rg is cancelled and true reports it; EOF alone ends the
	// report exactly at the budget. A final line riding io.EOF still counts as content.
	withheld := func(ln string, rerr error) bool {
		if rerr == nil || ln != "" {
			stopped = true
			_ = cmd.Cancel()
			return true
		}
		return false
	}

	switch {
	case mode == grepCount:
		var kept int     // matches shown across trimmed counts
		var trimmed bool // a count cut partway hides its remainder even when rg ends right after
		for {
			ln, rerr := r.ReadString('\n')
			if max > 0 && kept >= max { // budget spent on an earlier file: this read decides the outcome
				spent := withheld(ln, rerr) // a further count is one whole file withheld
				cut = spent || trimmed
				break
			}
			if rel, n, ok := splitCountLine(strings.TrimRight(relLine(root, ln), "\r\n")); ok && kept < max {
				take := min(n, max-kept)
				trimmed = trimmed || take < n // the rest of this count is withheld from display
				kept += take
				fmt.Fprintf(&b, "%s:%d\n", rel, take)
			}
			if rerr != nil { // rg finished naturally at or under the budget
				cut = trimmed
				break
			}
		}
	case blockMode:
		// Context rides whole blocks streamed from rg's JSON events, where match
		// and context lines are distinct: a pattern occurring in a printed path
		// cannot spend the budget on context. The byte bound only stops a runaway
		// context size; finalize bounds and names whatever it cuts.
		lim := GrepResultLimit()
		var shown int // matching lines streamed; context lines and separators ride uncounted
		var lastPath string
		var lastLine int
		var started bool
		dec := json.NewDecoder(stdout)
		for {
			var ev rgEvent
			if err := dec.Decode(&ev); err != nil { // io.EOF: the report is complete
				break
			}
			if ev.Type != "match" && ev.Type != "context" {
				continue // begin/end/summary records carry no output line
			}
			pth := grepRel(root, ev.Data.Path.String())
			contiguous := started && pth == lastPath && ev.Data.LineNumber == lastLine+1
			if max > 0 && shown >= max && (ev.Type == "match" || !contiguous) {
				// past the budget a further match, or context not trailing the final
				// block, proves content was withheld: stop scanning and name the cap.
				// Trailing context of the final block still rides, whole as streamed.
				stopped = true
				cut = true
				_ = cmd.Cancel()
				break
			}
			sep := ":" // match lines print path:line:text, context ones path-line-text
			if ev.Type == "context" {
				sep = "-"
			} else {
				shown++
			}
			txt := ev.Data.Lines.String()
			if !strings.HasSuffix(txt, "\n") {
				txt += "\n" // a final line without one would glue the next group separator
			}
			if started && (pth != lastPath || ev.Data.LineNumber != lastLine+1) {
				b.WriteString("--\n") // disjoint blocks separate, as rg's text printer emits
			}
			_, _ = fmt.Fprintf(&b, "%s%s%d%s%s", pth, sep, ev.Data.LineNumber, sep, txt)
			started, lastPath, lastLine = true, pth, ev.Data.LineNumber
			over := lim.Bytes > 0 && b.Len() >= lim.Bytes // the byte bound only stops a runaway context size
			if over {
				stopped = true
				_ = cmd.Cancel()
				break
			}
		}
	default: // content without context, or files: every line is one match
		var lines int // kept output lines, each equals a match here
		for {
			ln, rerr := r.ReadString('\n')
			if cut { // budget filled earlier: this read tells an exact fill from withholding
				cut = withheld(ln, rerr) // a further line is withheld whole past the cap
				break
			}
			if ln != "" {
				b.WriteString(relLine(root, ln))
				lines++
			}
			cut = max > 0 && lines >= max // the report filled at or before this line
			if rerr != nil {              // the stream ended with this line: nothing can follow to withhold
				cut = false
				break
			}
		}
	}

	waitErr := cmd.Wait() // reap, an error on every cancelled run, handled below
	var warn string       // benign stderr (partial-coverage warnings) to surface like the fallback does
	if !stopped && waitErr != nil {
		var ee *exec.ExitError
		if errors.As(waitErr, &ee) && ee.ExitCode() == 1 { // no matches: empty result, not an error
		} else {
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = waitErr.Error()
			}
			return "", false, "", fmt.Errorf("rg: %s", msg)
		}
	} else if !stopped { // benign end (exit 0/1): forward any warnings rg still emitted
		warn = strings.TrimSpace(stderr.String())
	}
	return strings.TrimRight(b.String(), "\n"), cut, warn, nil
}

// grepRel names a searched path relative to the search root. Searching a
// single file attributes hits to that file's own name rather than relTo's ".",
// so one explicit path reads like any other result line.
func grepRel(root, path string) string {
	if rel := relTo(root, path); rel != "." {
		return rel
	}
	return filepath.Base(path)
}

// relLine rewrites an rg output line's leading path from the absolute search
// root to root-relative shape, so both engines print one form. A single-file
// search (where every printed path is the root itself) names that file.
func relLine(root, ln string) string {
	tail := strings.TrimSuffix(ln, "\n")
	rest := strings.TrimPrefix(tail, root)
	if len(rest) == len(tail) { // unrooted line, e.g. a "--" context group separator
		return ln
	}
	switch {
	case rest == "" || rest[0] == ':':
		tail = filepath.Base(root) + rest
	case rest[0] == filepath.Separator:
		tail = rest[1:]
	default:
		return ln // not a rooted path; leave the line untouched
	}
	if strings.HasSuffix(ln, "\n") { // restore the newline ReadString kept
		tail += "\n"
	}
	return tail
}

// rgEvent is one record of rg --json output; only match and context records
// carry printable lines.
type rgEvent struct {
	Type string `json:"type"`
	Data struct {
		Path       rgText `json:"path"`
		LineNumber int    `json:"line_number"`
		Lines      rgText `json:"lines"`
	} `json:"data"`
}

// rgText is printed line content: text when valid UTF-8, base64 bytes otherwise.
type rgText struct {
	Text  string `json:"text"`
	Bytes []byte `json:"bytes"` // decoded from base64 by encoding/json
}

// String returns the line content, falling back to raw bytes for a non-UTF-8 line.
func (x rgText) String() string {
	if x.Text != "" || x.Bytes == nil {
		return x.Text
	}
	return string(x.Bytes)
}

// splitCountLine splits a count line into path and match count, false when the
// line is not one.
func splitCountLine(line string) (string, int, bool) {
	i := strings.LastIndex(line, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(line[i+1:])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return line[:i], n, true
}

// goGrep returns matched lines (with their real line numbers, context lines
// included around each match) and the total match count, capped at limit. A
// matching line reached first as another match's context is still flagged a
// match. ends[i] is the exclusive hits index where match i's context block
// ends, so a caller can trim hits to the first k matches.
func goGrep(content string, re *regexp.Regexp, ctxLines, limit int) (hits []grepHit, n int, ends []int) {
	if content == "" {
		return nil, 0, nil // an empty file has no lines, rg emits nothing for one
	}
	ctxLines = max(ctxLines, 0) // negative rides as none: the rg path only enables context past zero
	lines := strings.Split(content, "\n")
	if strings.HasSuffix(content, "\n") {
		lines = lines[:len(lines)-1] // the split's trailing element is past EOF, not a line
	}
	emitted := map[int]int{} // line index -> hits position it was emitted at
	for i, line := range lines {
		if limit > 0 && n >= limit {
			break
		}
		if !re.MatchString(line) {
			continue
		}
		lo := max(0, i-ctxLines)
		hi := min(len(lines)-1, i+ctxLines)
		for j := lo; j <= hi; j++ {
			if at, ok := emitted[j]; ok {
				if j == i { // rode along as earlier context first: promote to a match line
					hits[at].match = true
				}
				continue
			}
			emitted[j] = len(hits)
			hits = append(hits, grepHit{line: j + 1, text: lines[j], match: j == i})
		}
		n++
		ends = append(ends, len(hits))
	}
	return hits, n, ends
}

type grepHit struct {
	line  int
	text  string
	match bool // a match line rather than context around one
}

// binary reports whether data looks like a binary file.
func binary(data []byte) bool { return detect(data) == fileBinary }
