package projinit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildTask(t *testing.T) {
	t.Parallel()

	for _, want := range []string{"Makefile", ".github/workflows", "CONTRIBUTING.md",
		"build the project", "run its tests", "lint it", "Name the file each command came from"} {
		assert.Contains(t, buildTask, want)
	}
	assert.True(t, strings.HasSuffix(buildTask, summaryTail))
	assert.Contains(t, summaryTail, "never guess")
}

func TestDistillPrompts(t *testing.T) {
	t.Parallel()

	for name, prompt := range map[string]string{"new": distillNew, "update": distillUpdate} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, strings.HasPrefix(prompt, distillHeader))
			assert.True(t, strings.HasSuffix(prompt, distillRules))
			// brevity is the requirement; the file is read on every turn
			assert.Contains(t, prompt, "clear and concise")
			assert.Contains(t, prompt, "brevity is a feature")
			// every claim traces to the survey; nothing is invented
			assert.Contains(t, prompt, "Every claim must trace to something in the survey above")
			assert.Contains(t, prompt, "Never invent commands, conventions or code-style rules")
			// the write goes through the normal tool, so the barrier gates it
			assert.Contains(t, prompt, "Write the finished document to AGENTS.md with the write tool")

			// ajent's own code-style sections are project-specific and never copied
			assert.NotContains(t, prompt, "## Code Style")
			assert.NotContains(t, prompt, "zero-value initialization")
			assert.NotContains(t, prompt, "bulk.SliceFilter")
			assert.NotContains(t, prompt, "testify")
		})
	}

	t.Run("new_drafts", func(t *testing.T) {
		assert.Contains(t, distillNew, "## Project Overview")
		assert.Contains(t, distillNew, "## Commands")
		assert.Contains(t, distillNew, "## Architecture")
	})

	t.Run("update_corrects", func(t *testing.T) {
		assert.Contains(t, distillUpdate, "Make sure it is accurate")
		assert.Contains(t, distillUpdate, "source of truth")
		assert.Contains(t, distillUpdate, "correction pass, not a rewrite")
		assert.NotEqual(t, distillNew, distillUpdate)
	})
}
