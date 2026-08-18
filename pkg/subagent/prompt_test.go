package subagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestChildContractVerbatim pins the system snippet a child gets, so a wording
// change is deliberate and reviewed.
func TestChildContractVerbatim(t *testing.T) {
	t.Parallel()
	assert.Contains(t, childContract, "read-only tools: read, grep, find, ls")
	assert.Contains(t, childContract, "FINAL assistant message must be a single, self-contained summary")
	assert.Contains(t, childContract, "Do not emit tool calls in that final message")
}

// TestContinueNudgeVerbatim pins the empty-summary nudge.
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
	s := childSnippets()
	if assert.Len(t, s, 1) {
		assert.Equal(t, childContract, s[0])
	}
}
