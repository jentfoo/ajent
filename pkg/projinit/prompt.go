package projinit

import (
	"fmt"
	"strings"
)

// Survey prompts, from docs/prompt-design.md. Stage 2's tasks go to read-only
// sub-agents; stage 3's instruction is what turns their summaries into the file.
const (
	// summaryTail closes every sub-agent task: the parent pastes prose, not dumps.
	summaryTail = `

End with a summary written to be pasted into an AGENTS.md: prose, not a raw dump. Report only what you actually read — never guess, and never generalise from convention.`

	buildTask = `Survey how this project is built, tested and linted.

Read the Makefile or equivalent build file, any CI configuration (.github/workflows or this project's equivalent), and CONTRIBUTING.md if it exists.

Report exactly which commands build the project, run its tests and lint it, and what each one expects: toolchains and versions, environment variables, generated files, and any setup step that must run first. Name the file each command came from.` + summaryTail

	// codeTaskFmt takes one disjoint slice of the tree; another agent covers the rest.
	codeTaskFmt = `Survey this slice of the repository: %s

Stay inside those paths. Another sub-agent covers the rest of the tree.

Report what each package or module does, the dependency edges between them, the key entry points, and any invariant or constraint worth recording for someone changing this code.` + summaryTail
)

// The distillation instruction. Both variants share a header naming the survey as
// data and a closing rule set; only the middle — draft versus correct — differs.
const (
	distillHeader = `The messages above are a survey of this repository: the files read directly, plus one summary per read-only sub-agent that investigated the build and the code.

`

	distillRules = `

Rules:
- Every claim must trace to something in the survey above. Never invent commands, conventions or code-style rules that were not reported.
- Keep the wording clear and concise. This file is read on every turn, so brevity is a feature.
- Write the finished document to AGENTS.md with the write tool, then stop. Do not repeat it in your reply.`

	distillNew = distillHeader + `Write AGENTS.md for this project — the instruction file an agent reads at the start of every turn. Use these sections, in order:

## Project Overview
One paragraph: what this project is.

## Commands
The build, test and lint commands from the survey, exactly as reported.

## Architecture
Where the code lives, what each part does, and the design notes and invariants worth knowing before changing it.

Add a further section for any convention the survey actually observed.` + distillRules

	distillUpdate = distillHeader + `AGENTS.md already exists and was read above. Make sure it is accurate.

The survey is the source of truth: correct anything it contradicts, add what it shows is missing, and keep the existing structure and wording where they are still right. This is a correction pass, not a rewrite.` + distillRules
)

// codeTask returns the survey task for one disjoint slice of the tree.
func codeTask(paths []string) string {
	return fmt.Sprintf(codeTaskFmt, strings.Join(paths, ", "))
}
