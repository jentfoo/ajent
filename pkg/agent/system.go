package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Environment is the machine facts the system prompt states. It is a plain
// struct so tests can inject it and callers can layer project instructions
// (AGENTS.md) on top.
type Environment struct {
	Cwd  string
	OS   string
	Date string // day granularity, so the prompt cache survives
}

// DetectEnvironment reads the facts about this machine that a coding agent is
// expected to know. The cwd listing is captured at startup and stays fixed for
// the session so the system block remains cache-stable.
func DetectEnvironment() Environment {
	cwd, _ := os.Getwd()
	return Environment{
		Cwd:  cwd,
		OS:   runtime.GOOS + "/" + runtime.GOARCH,
		Date: time.Now().Format("2006-01-02"),
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
// the prompt cache survives. Project instructions and snippets are explicit
// inputs (not read here) so callers control when they reload and tests can assert
// byte equality across calls with equal inputs.
func buildSystem(s *State, env Environment, proj []ProjectInstruction, snippets []string) llm.BlockList {
	var b strings.Builder

	b.WriteString(identityLine(cwdOrDot(env.Cwd)))
	b.WriteString("You help by reading files, running commands, editing code and writing new files.\n\n")

	// guidelines first, then environment facts per the design doc
	b.WriteString(buildGuidelines(s.Tools))

	buildEnvironmentFacts(&b, env)

	if len(proj) > 0 {
		writeProjectInstructions(&b, proj)
	}

	for _, snip := range snippets {
		b.WriteString("\n" + strings.TrimSuffix(snip, "\n") + "\n")
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

	// search hint only when bash is present and no dedicated find/grep tool is
	if slices.Contains(names, "bash") && !slices.Contains(names, "find") && !slices.Contains(names, "grep") {
		b.WriteString("- Use bash for file operations like ls, grep, find\n")
	}

	return b.String() + "\n"
}

// buildEnvironmentFacts appends the working directory and an ls-style listing of
// it. Empty values are omitted rather than emitted as "unknown"; shell and git
// status are deliberately absent, and only Date varies within a session so the
// provider prompt cache survives.
func buildEnvironmentFacts(b *strings.Builder, env Environment) {
	fmt.Fprintf(b, "Working directory: %s\n", cwdOrDot(env.Cwd))
	if env.OS != "" {
		fmt.Fprintf(b, "Platform: %s\n", env.OS)
	}
	fmt.Fprintf(b, "Date: %s\n", env.Date)
	if entries := listCwd(cwdOrDot(env.Cwd)); len(entries) > 0 {
		b.WriteString("Directory contents:\n")
		for _, e := range entries {
			fmt.Fprintf(b, "  %s\n", e)
		}
	}
}

func cwdOrDot(cwd string) string {
	if cwd == "" {
		return "."
	}
	return cwd
}

// listCwd returns the sorted names of entries in dir, dirs marked with a trailing
// slash like ls. Absent or unreadable dirs yield nil so the section is omitted.
func listCwd(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
