package tools

import (
	"context"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
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
	policy  PathPolicy
	forceGo bool // skip rg even when present, so tests exercise the Go fallback
}

var _ agent.Tool = (*grepTool)(nil)

func (t *grepTool) Name() string { return "grep" }

func (t *grepTool) Label(agent.ToolCall) string {
	return "grep"
}

func (t *grepTool) Description() string {
	return "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Respects .gitignore."
}
func (t *grepTool) Schema() llm.ToolSchema { return llm.ToolSchema{Parameters: SchemaOf[grepParams]()} }
func (t *grepTool) Mode() agent.ExecutionMode {
	return agent.ModeParallel
}

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
		out, rgErr := runRg(cwd, p, mode, max)
		if rgErr != nil {
			return resultErr("grep: " + rgErr.Error()), nil
		}
		elided, _ := Elide(out, grepLimit)
		return agent.ToolResult{Content: llmBlock(elided)}, nil
	}

	return t.goSearch(cwd, p, mode, re, max), nil
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

// goSearch walks cwd with the compiled matcher when rg is unavailable.
func (t *grepTool) goSearch(cwd string, p grepParams, mode string, re *regexp.Regexp, max int) agent.ToolResult {
	var matches []string
	counts := map[string]int{}
	remaining := max

	for _, path := range listAllFiles(cwd) {
		if p.Glob != "" && !matchGlob(p.Glob, relTo(cwd, path)) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil || binary(data) {
			continue
		}
		rel := relTo(cwd, path)
		hits, n := goGrep(string(data), re, p.Context, remaining)
		switch mode {
		case grepCount:
			if n > 0 {
				counts[rel] = n
			}
		case grepFiles:
			if len(hits) > 0 {
				matches = append(matches, rel)
				remaining--
			}
		default:
			for _, h := range hits {
				matches = append(matches, fmt.Sprintf("%s:%d: %s", rel, h.line, strings.TrimSpace(h.text)))
			}
			remaining -= n
		}
		if remaining <= 0 {
			break
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
	outStr, _ := Elide(b.String(), GrepResultLimit())
	return agent.ToolResult{Content: llmBlock(strings.TrimRight(outStr, "\n"))}
}

// rgOnPath reports whether ripgrep is available.
func rgOnPath() bool { return lookPath("rg") }

// runRg shells out to ripgrep and returns its output for the requested mode.
// Exit status 1 means "no matches"; anything else with stderr is an error the
// model should see rather than a silent empty result.
func runRg(cwd string, p grepParams, mode string, max int) (string, error) {
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
	if p.Context > 0 && (mode == "" || mode == grepContent) {
		args = append(args, "-C", strconv.Itoa(p.Context))
	}
	if p.Glob != "" {
		args = append(args, "--glob", p.Glob)
	}
	if mode != grepCount {
		args = append(args, "-m", strconv.Itoa(max))
	}
	args = append(args, "--", p.Pattern, cwd)

	out, err := runCaptured("rg", args...)
	if err != nil {
		return "", err
	}
	return truncateLines(out, max), nil
}

// truncateLines keeps at most max lines of s so a per-file -m bound still
// respects the overall match budget.
func truncateLines(s string, max int) string {
	if max <= 0 {
		return s
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= max {
		return s
	}
	return strings.Join(lines[:max], "\n")
}

// goGrep returns matched lines (with their real line numbers, context lines
// included around each match) and the total match count, capped at max.
func goGrep(content string, re *regexp.Regexp, ctxLines, limit int) (hits []grepHit, n int) {
	lines := strings.Split(content, "\n")
	emitted := map[int]bool{}
	for i, line := range lines {
		if limit > 0 && n >= limit {
			break
		}
		if !re.MatchString(line) {
			continue
		}
		n++
		lo := max(0, i-ctxLines)
		hi := min(len(lines)-1, i+ctxLines)
		for j := lo; j <= hi; j++ {
			if emitted[j] {
				continue
			}
			emitted[j] = true
			hits = append(hits, grepHit{line: j + 1, text: lines[j]})
		}
	}
	return hits, n
}

type grepHit struct {
	line int
	text string
}

// listAllFiles walks cwd returning every regular file path, skipping VCS and
// dependency directories.
func listAllFiles(cwd string) []string {
	var out []string
	for _, p := range allWalk(cwd) {
		if !IsSkippedDir(p) {
			out = append(out, p)
		}
	}
	return out
}

// binary reports whether data looks like a binary file.
func binary(data []byte) bool { return detect(data) == fileBinary }
