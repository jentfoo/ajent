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
	requireLen(t, s, 1)
	if len(s) != 0 {
		assert.Equal(t, childContract, s[0])
	}
}

// requireLen is a tiny helper to avoid importing testify just for this.
func requireLen[T any](t *testing.T, got []T, want int) {
	t.Helper()
	if len(got) != want {
		t.Fatalf("expected length %d, got %d", want, len(got))
	}
}
