package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChildContractVerbatim(t *testing.T) {
	t.Parallel()

	assert.Contains(t, childContract(true), "read-only tools: read, grep, find, ls")
	assert.Contains(t, childContract(false), "read-only tools: read, grep, find, ls")
	assert.NotContains(t, childContract(false), "git_")
	for _, name := range gitToolNames {
		assert.Contains(t, childContract(true), name)
	}
	assert.Contains(t, childContract(true), "FINAL assistant message must be a single, self-contained summary")
	assert.Contains(t, childContract(false), "Do not emit tool calls in that final message")
}

func TestContinueNudgeVerbatim(t *testing.T) {
	t.Parallel()

	assert.Contains(t, continueNudge, "no summary text")
	assert.Contains(t, continueNudge, "no tool calls")
}

func TestTaskPromptFramesInstructionsAndTask(t *testing.T) {
	t.Parallel()

	p := taskPrompt("find it", "")
	assert.Equal(t, "Task:\nfind it", p)

	q := taskPrompt("  find \n it ", "\nbe concise\n")
	assert.Contains(t, q, "Extra instructions:\nbe concise")
	assert.Contains(t, q, "Task:\nfind \n it") // inner whitespace preserved
}

func TestChildSnippetsIsSingleContract(t *testing.T) {
	t.Parallel()

	s := childSnippets(true)
	if assert.Len(t, s, 1) {
		assert.Equal(t, childContract(true), s[0])
	}
}
