package tools

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

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

// selfBounding: grep bounds and spills its own results.
func (*grepTool) selfBounding() {}

// Execute runs the search, bounded by the limit and GrepResult.
func (t *grepTool) Execute(ctx context.Context, call agent.ToolCall, _ agent.Output) (agent.ToolResult, error) {
	var p grepParams
	if err := decode(call.Input, &p); err != nil {
		return resultErr("bad args: " + err.Error()), nil
	}
	if p.Pattern == "" {
		return resultErr("grep needs a non-empty pattern"), nil
	}
	cwd, err := t.policy.Resolve(argPath(p.Path))
	if err != nil {
		return resultErr(err.Error()), nil
	}

	mode := p.Mode
	switch mode {
	case "", grepContent, grepFiles, grepCount:
	default:
		return resultErr("grep mode must be files, content or count"), nil
	}

	// compile first: a bad regex must surface as an error, not empty results
	re, err := p.compile()
	if err != nil {
		return resultErr("grep: " + err.Error()), nil
	}

	grepLimit := GrepResultLimit()
	max := p.Limit
	if max <= 0 {
		max = grepLimit.Lines
	}

	if !t.forceGo && rgOnPath() {
		out, cut, warn, rgErr := runRg(ctx, cwd, p, mode, max)
		if rgErr != nil {
			return resultErr("grep: " + rgErr.Error()), nil
		}
		blockMode := (mode == "" || mode == grepContent) && p.Context > 0
		note := capNote(out, p.Limit, max, mode)
		switch {
		case blockMode && cut:
			// context rides whole blocks bounded by the output limit, never an exact match
			// count; a cut means matches beyond GrepResult went unseen even under an explicit
			// max, so always name it.
			note = resultCapNote(GrepResultLimit().Lines)
		case mode == grepCount || (cut && p.Limit <= 0): // budget spent; mirror the fallback's note
			note = resultCapNote(max)
		}
		if warn != "" { // rg warned about paths it could not read; mirror the fallback's note
			note = joinNotes(note, "rg: "+strutil.FirstLine(warn))
		}
		// Display mirrors the model-visible text
		return t.finalize(out, note), nil
	}

	return t.goSearch(ctx, cwd, p, mode, re, max), nil
}

// capNote names the default match cap when enumeration reached it without an
// explicit limit, so a stopped search is never mistaken for complete.
func capNote(out string, explicit, max int, mode string) string {
	if explicit > 0 || mode == grepCount || countLines(out) < max {
		return ""
	}
	return resultCapNote(max)
}

// resultCapNote names the match cap a truncated result hit. Count output notes
// whenever anything was withheld: a trimmed count reads as authoritative in a
// way cut-off content lines do not.
func resultCapNote(max int) string {
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
// run concurrently in walk order; the assembled output matches the sequential
// walk byte for byte, budget included.
func (t *grepTool) goSearch(ctx context.Context, cwd string, p grepParams, mode string, re *regexp.Regexp, max int) agent.ToolResult {
	type fileMatch struct {
		rel  string
		hits []grepHit
		ends []int // ends[i]: exclusive hits index at match i's context block end
		n    int
		skip bool // inspected but binary or unreadable; can hold no matches
	}
	var paths []string
	for _, path := range repoFiles(ctx, cwd) { // .gitignore semantics on the fallback too
		if p.Glob == "" || matchGlob(p.Glob, relTo(cwd, path)) {
			paths = append(paths, path)
		}
	}

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
					m.skip = true // inspected but unreadable; report it as not searched
					results[i-lo] = m
					return
				}
				defer func() { _ = f.Close() }()
				head := make([]byte, sniffLen)
				n, _ := io.ReadFull(f, head) // n < len(head) when the whole file fit
				if binary(head[:n]) {
					m.skip = true // inspected but unsearchable; report it as not searched
					results[i-lo] = m
					return
				}
				var data []byte
				switch {
				case n < len(head): // whole file fit in the sniff window: no remainder to read
					data = head[:n]
				default:
					// a text file continues on the same handle; binaries never load past the
					// sniff, so a big binary is rejected without its full bytes ever allocated.
					var buf bytes.Buffer // accumulates head + remainder into one backing array
					buf.Write(head)
					if _, rerr := io.Copy(&buf, f); rerr != nil {
						m.skip = true // inspected but unreadable; report it as not searched
						results[i-lo] = m
						return
					}
					data = buf.Bytes()
				}
				m.rel = relTo(cwd, path)
				m.hits, m.n, m.ends = goGrep(normalizeToLF(string(data)), re, p.Context, budget)
				results[i-lo] = m
			}(i, paths[i])
		}
		wg.Wait()
		return results
	}

	var matches []string
	counts := map[string]int{}
	var skipped int // files inspected but binary or unreadable, so not actually searched
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
				n = remaining // trim to the shared budget; files mode spends one per file
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
				for _, h := range m.hits {
					matches = append(matches, fmt.Sprintf("%s:%d: %s", m.rel, h.line, strings.TrimSpace(h.text)))
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
	note := capNote(trimmed, p.Limit, max, mode)
	if mode == grepCount && remaining <= 0 {
		note = resultCapNote(max) // the budget was spent; a trimmed count reads as authoritative
	}
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
	out = normalizeToLF(out) // rg and go paths both carry \r on CRLF files; LF-only to the model
	text, _ := truncateOutput(t.sessionID, ToolGrep, out, GrepResultLimit(), "")
	if note != "" {
		text += "\n" + note
	}
	return agent.ToolResult{Content: llmBlock(text), Display: text}
}

// rgOnPath reports whether ripgrep is available.
func rgOnPath() bool { return lookPath("rg") }

// runRg streams ripgrep output until the report is complete, then cancels rg
// so a deep tree with many matches does not keep scanning. Returns the result,
// whether the budget was spent (cut), any benign stderr warning (warn) to mirror
// the fallback's incomplete-coverage note, and any failure; exit status 1 means
// "no matches" but an early cancel ignores whatever exit code follows.
func runRg(ctx context.Context, cwd string, p grepParams, mode string, max int) (string, bool, string, error) {
	args := []string{"--no-heading", "--color=never"}
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
	blockMode := contentMode && p.Context > 0 // whole blocks: budget is output size, not match count
	if blockMode {
		args = append(args, "-C", strconv.Itoa(p.Context))
	}
	if p.Glob != "" {
		args = append(args, "--glob", p.Glob)
	}
	if mode != grepCount {
		args = append(args, "-m", strconv.Itoa(max)) // per-file bound; the stream stops globally
	}
	args = append(args, "--", p.Pattern, cwd)

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
	stopped := false                         // true once the report filled and rg was cancelled early
	var cut bool                             // true once the budget is spent
	r := bufio.NewReaderSize(stdout, 64<<10) // no line-length cap; minified lines survive
	switch {
	case mode == grepCount:
		kept := 0 // matches shown across trimmed counts
		for {
			ln, rerr := r.ReadString('\n')
			if rel, n, ok := splitCountLine(strings.TrimRight(ln, "\r\n")); ok && kept < max {
				take := min(n, max-kept)
				kept += take
				fmt.Fprintf(&b, "%s:%d\n", rel, take)
			}
			cut = kept >= max // budget spent (whether or not more files follow)
			if rerr != nil {  // rg finished naturally
				break
			}
			if cut { // a further file would be withheld whole; stop scanning the tree
				stopped = true
				_ = cmd.Cancel()
				break
			}
		}
	case blockMode:
		// rg never marks context vs matched lines, so a strict line cut mid-block
		// would leave dangling context. Bound the stream by the output limit and
		// cancel there instead; finalize then names anything beyond GrepResult.
		lim := GrepResultLimit()
		for {
			ln, rerr := r.ReadString('\n')
			if ln != "" {
				b.WriteString(ln)
			}
			cut = lim.Lines > 0 && countLines(b.String()) >= lim.Lines ||
				lim.Bytes > 0 && b.Len() >= lim.Bytes
			if rerr != nil { // rg finished naturally with a complete report
				break
			}
			if cut {
				stopped = true
				_ = cmd.Cancel()
				break
			}
		}
	default: // content without context, or files: every line is one match
		var lines int // kept output lines; each equals a match here
		for {
			ln, rerr := r.ReadString('\n')
			if ln != "" {
				b.WriteString(ln)
				lines++
			}
			cut = max > 0 && lines >= max // the report filled at or before this line
			if rerr != nil {              // rg finished naturally with fewer than budget results
				break
			}
			if cut {
				stopped = true // stop rg scanning the rest of the tree once the report is complete
				_ = cmd.Cancel()
				break
			}
		}
	}

	waitErr := cmd.Wait() // reap; a cancelled run always reports an error, handled below
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
// included around each match) and the total match count, capped at limit.
// ends[i] is the exclusive hits index where match i's context block ends, so a
// caller can trim hits to the first k matches.
func goGrep(content string, re *regexp.Regexp, ctxLines, limit int) (hits []grepHit, n int, ends []int) {
	lines := strings.Split(content, "\n")
	emitted := map[int]bool{}
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
			if emitted[j] {
				continue
			}
			emitted[j] = true
			hits = append(hits, grepHit{line: j + 1, text: lines[j]})
		}
		n++
		ends = append(ends, len(hits))
	}
	return hits, n, ends
}

type grepHit struct {
	line int
	text string
}

// binary reports whether data looks like a binary file.
func binary(data []byte) bool { return detect(data) == fileBinary }
