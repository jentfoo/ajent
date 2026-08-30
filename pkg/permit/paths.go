package permit

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/tools"
)

// writeScope is where auto+write may write without a prompt.
type writeScope struct {
	cwd   string   // relative paths resolve against this directory
	roots []string // canonicalized roots; a path must land under one
}

// newWriteScope canonicalises cwd and each extra root, dropping any that fail.
// Roots resolve like call paths, so a root matches its symlinked form; a zero
// scope allows nothing.
func newWriteScope(cwd string, extra ...string) writeScope {
	s := writeScope{cwd: cwd}
	for _, r := range append([]string{cwd}, extra...) {
		if r == "" {
			continue
		}
		full, err := tools.PathPolicy{Cwd: cwd}.Resolve(r)
		if err != nil {
			continue
		}
		s.roots = append(s.roots, full)
	}
	return s
}

// callPath extracts the path field shared by write and edit, returning empty when
// it cannot be decoded so a scope check fails safe to the prompt path.
func callPath(input json.RawMessage) string {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return ""
	}
	return p.Path
}

// bashCwd extracts the working directory a bash call declares; empty when absent.
// The shell runs there, so it rebases every relative path in the command.
func bashCwd(input json.RawMessage) string {
	var p struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(input, &p); err != nil {
		return ""
	}
	return p.Cwd
}

// contains reports whether the already-resolved path full sits at or under a root.
func (s writeScope) contains(full string) bool {
	for _, r := range s.roots {
		if full == r || strings.HasPrefix(full, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// allows reports whether call writes only inside the scope: a core writer on an
// in-scope path, or a shell line that only reads or makes bounded directory
// changes there.
func (s writeScope) allows(call agent.ToolCall) bool {
	if len(s.roots) == 0 {
		return false
	}
	if call.Name == bashTool {
		rebased, ok := s.rebase(bashCwd(call.Input))
		if !ok {
			return false // the shell would run somewhere the scope does not cover
		}
		return rebased.allowsCommand(bashCommand(call.Input))
	}
	if _, ok := coreWriteTools[call.Name]; !ok {
		return false
	}
	return s.inScope(callPath(call.Input))
}

// rebase returns the scope a command declaring cwd runs under. An empty cwd keeps
// the session directory; anything outside the roots is refused outright.
func (s writeScope) rebase(cwd string) (writeScope, bool) {
	if cwd == "" {
		return s, true
	}
	full, ok := s.resolveInScope(cwd)
	if !ok {
		return writeScope{}, false
	}
	return writeScope{cwd: full, roots: s.roots}, true
}

// shellExpansion are characters the shell resolves after this check, so they
// can name a path the gate never saw: $ and ` substitute, and {} split one
// token into several words.
const shellExpansion = "$`{}"

// vcsDirs hold repository metadata that executes or rewrites history — a hook, a
// config alias, core.hooksPath. They sit under the roots but are never unattended.
var vcsDirs = bulk.SliceToSet([]string{".git", ".hg", ".svn"})

// resolveInScope resolves an unresolved path argument and reports whether it lands
// inside the scope. Empty, unresolvable, glob or expanding paths never do, nor does
// anything inside a VCS metadata directory.
func (s writeScope) resolveInScope(p string) (string, bool) {
	if p == "" || strings.ContainsAny(p, shellExpansion) || tools.HasGlob(p) {
		return "", false // what it names cannot be known before the shell expands it
	}
	full, err := tools.PathPolicy{Cwd: s.cwd}.Resolve(p)
	if err != nil || !s.contains(full) {
		return "", false
	}
	for _, part := range strings.Split(full, string(filepath.Separator)) {
		if _, ok := vcsDirs[part]; ok {
			return "", false
		}
	}
	return full, true
}

// inScope reports whether an unresolved path argument lands inside the scope.
func (s writeScope) inScope(p string) bool {
	_, ok := s.resolveInScope(p)
	return ok
}

// maxCdSegments bounds how many directory changes one line may make before the set
// of possible working directories stops being worth tracking.
const maxCdSegments = 4

// allowsCommand reports whether a bash line only reads or makes bounded
// directory changes inside the scope; redirects and substitution fail closed.
// A cd moves the baseline later relative paths resolve against, so both the
// old and new directory stay candidates.
func (s writeScope) allowsCommand(cmd string) bool {
	sc := scanCommand(cmd)
	if sc.HasUnsafeOp || len(sc.Segments) == 0 {
		return false
	}
	bases := []string{s.cwd}
	var cds int
	return forEachSegment(sc, func(seg, raw string) bool {
		if h, ok := headOf(seg); ok && h == "cd" {
			cds++
			if cds > maxCdSegments {
				return false
			}
			next, ok := s.cdTargets(bases, raw)
			if !ok {
				return false
			}
			bases = append(bases, next...)
			return true
		}
		return segmentIsReadOnly(seg, raw) || s.segmentWritesInScope(bases, seg, raw)
	})
}

// cdTargets returns the directories a cd segment may land in, resolved against
// every baseline already possible; ok is false when any lands outside the scope
// or the target cannot be named (a bare cd goes home, cd - is unknowable).
func (s writeScope) cdTargets(bases []string, raw string) ([]string, bool) {
	toks := tokenizeRaw(raw)
	if len(toks) != 2 || toks[1] == "-" {
		return nil, false
	}
	var out []string
	for _, b := range bases {
		full, ok := s.from(b).resolveInScope(toks[1])
		if !ok {
			return nil, false
		}
		if !slices.Contains(out, full) {
			out = append(out, full)
		}
	}
	return out, true
}

// from returns the same scope resolving relative paths against base.
func (s writeScope) from(base string) writeScope {
	return writeScope{cwd: base, roots: s.roots}
}

// inScopeFrom reports whether p lands inside the scope from every possible baseline,
// so a path is refused unless it is safe however the cd chain actually played out.
func (s writeScope) inScopeFrom(bases []string, p string) bool {
	for _, b := range bases {
		if !s.from(b).inScope(p) {
			return false
		}
	}
	return true
}

// topComponent returns the shallowest component of p — the highest directory an
// ancestor-removing flag can reach. Containment is closed downward, so checking it
// covers every deeper ancestor.
func topComponent(p string) string {
	sep := string(filepath.Separator)
	abs := strings.HasPrefix(p, sep)
	rest := strings.TrimPrefix(p, sep)
	if i := strings.Index(rest, sep); i >= 0 {
		rest = rest[:i]
	}
	if abs {
		return sep + rest
	}
	return rest
}

// segmentWritesInScope reports whether one segment is a workspace write command
// whose every path argument lands inside the scope from every possible baseline.
func (s writeScope) segmentWritesInScope(bases []string, seg, raw string) bool {
	head, ok := headOf(seg)
	if !ok {
		return false
	}
	spec, ok := workspaceWriteCommands[head]
	if !ok {
		return false
	}
	toks := tokenizeRaw(raw) // raw keeps quoted paths intact
	if len(toks) < 2 || stripPath(toks[0]) != head {
		return false
	}
	args, ancestors, ok := spec.parseFlags(toks[1:])
	if !ok || len(args) == 0 {
		return false
	}
	for _, p := range args {
		if !s.inScopeFrom(bases, p) {
			return false
		}
		// -p walks up removing empty ancestors, so the highest one it reaches must
		// be in scope too; everything between sits under it
		if ancestors && !s.inScopeFrom(bases, topComponent(p)) {
			return false
		}
	}
	return true
}

// parseFlags splits a command's arguments into operands, reporting whether any
// flag makes it act on ancestors. ok is false on an unrecognised flag — one
// that consumes a value is indistinguishable from an operand.
func (c workspaceWriteCommand) parseFlags(args []string) (operands []string, ancestors, ok bool) {
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if !strings.HasPrefix(tok, "-") {
			operands = append(operands, tok)
			continue
		}
		name, joined := tok, false
		if eq := strings.Index(tok, "="); eq >= 0 {
			name, joined = tok[:eq], true
		}
		switch {
		case slices.Contains(c.valueFlags, name):
			if !joined {
				i++ // its value is not a path
			}
		case slices.Contains(c.boolFlags, name):
			if slices.Contains(c.ancestorFlags, name) {
				ancestors = true
			}
		default:
			return nil, false, false
		}
	}
	return operands, ancestors, true
}
