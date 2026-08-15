package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
)

// gitTimeout bounds a single git probe so an unreachable repo cannot stall the
// system prompt.
const gitTimeout = 2 * time.Second

// Environment is the machine facts the system prompt states. It is a plain
// struct so tests can inject it and callers can layer project instructions
// (AGENTS.md) on top.
type Environment struct {
	Cwd    string
	OS     string
	Shell  string
	Date   string // day granularity, so the prompt cache survives
	Branch string
	Dirty  bool
}

// DetectEnvironment reads the facts about this machine that a coding agent is
// expected to know. Git failures fall back silently to empty.
func DetectEnvironment() Environment {
	cwd, _ := os.Getwd()
	return Environment{
		Cwd:    cwd,
		OS:     runtime.GOOS + "/" + runtime.GOARCH,
		Shell:  shellName(),
		Date:   time.Now().Format("2006-01-02"),
		Branch: gitBranch(),
		Dirty:  gitDirty(),
	}
}

// ProjectInstruction is one AGENTS.md file injected into the system block,
// provenance-marked with its absolute source path.
type ProjectInstruction struct {
	Path string // absolute source path, used for <project_instructions path=...>
	Body string // verbatim file contents
}

// agentsFileName is the single instruction file ajent recognises in each source
// directory. Discovery covers only the launch cwd and the user-global config
// dir; ancestor walking is not supported.
const agentsFileName = "AGENTS.md"

// LoadProjectInstructions reads <dir>/AGENTS.md from each of dirs, in order,
// returning a provenance-marked instruction per existing file (global first,
// then project). Empty dirs and absent files are skipped; nil means none were
// found. A read error other than a file being absent is returned for callers to
// surface.
func LoadProjectInstructions(dirs ...string) ([]ProjectInstruction, error) {
	var proj []ProjectInstruction
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, agentsFileName)
		data, err := os.ReadFile(path)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			return nil, err
		}
		proj = append(proj, ProjectInstruction{Path: path, Body: string(data)})
	}
	return proj, nil
}

// buildSystem returns the system blocks for state, stable across a session so
// the prompt cache survives. Project instructions are an explicit input (not
// read here) so callers control when they reload and tests can assert byte
// equality across calls with equal inputs.
func buildSystem(s *State, env Environment, proj []ProjectInstruction) llm.BlockList {
	var b strings.Builder

	b.WriteString(identityLine(cwdOrDot(env.Cwd)))
	b.WriteString("You help by reading files, running commands, editing code and writing new files.\n\n")

	// guidelines first, then environment facts per the design doc
	b.WriteString(buildGuidelines(s.Tools))

	buildEnvironmentFacts(&b, env)

	if len(proj) > 0 {
		writeProjectInstructions(&b, proj)
	}

	return llm.BlockList{llm.TextBlock{Text: b.String()}}
}

// writeProjectInstructions appends provenance-marked AGENTS.md blocks inside a
// <project_context> wrapper. The format mirrors what other agents send so the
// model can tell project instructions from conversation.
func writeProjectInstructions(b *strings.Builder, proj []ProjectInstruction) {
	b.WriteString("\n<project_context>\n\n")
	b.WriteString("Project-specific instructions and guidelines:\n\n")
	for _, p := range proj {
		fmt.Fprintf(b, "<project_instructions path=%q>\n%s\n</project_instructions>\n",
			p.Path, strings.TrimSuffix(p.Body, "\n"))
	}
	b.WriteString("</project_context>")
}

// identityLine is the neutral first sentence naming the working directory. It is
// deliberately not a persona and claims only what the toolset can do.
func identityLine(cwd string) string {
	return fmt.Sprintf("You are an expert coding assistant that works in the repository at %s.\n", cwd)
}

// buildGuidelines returns the guideline block: the always-included bullets first,
// then any derived from the enabled toolset. The derivation rule is that
// guidelines never name a tool that is not present.
func buildGuidelines(names []string) string {
	var b strings.Builder
	b.WriteString("Guidelines:\n")
	b.WriteString("- Be concise in your responses\n")
	b.WriteString("- Show file paths clearly when working with files\n")
	b.WriteString("- Answer in the same language as the user's message\n")

	// search hint only when bash is present and no dedicated find/grep tool is
	if slices.Contains(names, "bash") && !slices.Contains(names, "find") && !slices.Contains(names, "grep") {
		b.WriteString("- Use bash for file operations like ls, grep, find\n")
	}

	return b.String() + "\n"
}

// buildEnvironmentFacts appends clean structured machine-fact lines. Empty values
// are omitted entirely rather than emitted as "unknown", and only Date varies
// within a session so the provider prompt cache survives.
func buildEnvironmentFacts(b *strings.Builder, env Environment) {
	fmt.Fprintf(b, "Working directory: %s\n", cwdOrDot(env.Cwd))
	if env.OS != "" {
		fmt.Fprintf(b, "Platform: %s\n", env.OS)
	}
	if env.Shell != "" {
		fmt.Fprintf(b, "Shell: %s\n", env.Shell)
	}
	fmt.Fprintf(b, "Date: %s\n", env.Date)
	if env.Branch != "" {
		fmt.Fprintf(b, "Git branch: %s", env.Branch)
		if env.Dirty {
			b.WriteString(" (dirty)")
		}
		b.WriteByte('\n')
	}
}

func cwdOrDot(cwd string) string {
	if cwd == "" {
		return "."
	}
	return cwd
}

// shellName returns a short name for the login shell, or empty when unknown.
func shellName() string {
	shell := os.Getenv("SHELL")
	switch {
	case strings.HasSuffix(shell, "/bash"):
		return "bash"
	case strings.HasSuffix(shell, "/zsh"):
		return "zsh"
	case strings.HasSuffix(shell, "/fish"):
		return "fish"
	default:
		return ""
	}
}

// gitBranch returns the current branch name via git rev-parse.
func gitBranch() string {
	out := runGit("rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out)
}

// gitDirty reports whether the working tree has uncommitted changes.
func gitDirty() bool { return gitDirtyIn("") }

// gitDirtyIn is gitDirty scoped to a directory, for tests. Untracked files do
// not count as dirty since they are new rather than modified.
func gitDirtyIn(dir string) bool {
	out := runGitIn(dir, "status", "--porcelain")
	for _, line := range strings.Split(out, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "??") {
			return true
		}
	}
	return false
}

// runGit invokes git quietly, returning stdout or empty on any failure.
func runGit(args ...string) string {
	return runGitIn("", args...)
}

// runGitIn runs git in dir, returning stdout or empty on any failure. The empty
// dir means the current working directory; tests pass a temp dir to exercise
// the silent-failure path.
func runGitIn(dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return ""
	}
	return buf.String()
}
