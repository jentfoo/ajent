package session

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadInfoCountsActiveBranchOnly regresses the fork over-count: after a
// branch, readInfo must describe only the persisted head's chain, so resume
// metadata matches what resuming would actually rebuild.
func TestReadInfoCountsActiveBranchOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-01T00-00-00Z-abcd.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)

	e1, _ := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "first on main")})
	e2, _ := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "reply one")})

	// a fork from the first message grows two more messages that must NOT count
	w.SetHead(e1.ID)
	forkA, _ := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "fork prompt")})
	_, _ = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "fork reply")})
	w.SetHead(e2.ID) // head the resume picker would actually use
	require.NoError(t, w.Close())

	info, ok := readInfo(p)
	require.True(t, ok)

	// main chain has 3 messages (session entry excluded from count): e1,e2 + none;
	// but our loop counts TypeMessage only: e1 and e2 = 2.
	assert.Equal(t, 2, info.Messages, "the fork's messages are not counted")
	assert.Contains(t, info.First, "first on main")

	_ = forkA
}

// TestReadInfoBranchPointsAtFork verifies metadata reflects a head that sits on
// the abandoned fork rather than the file tail.
func TestReadInfoBranchPointsAtFork(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion})
	require.NoError(t, err)

	e1, _ := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "main prompt")})
	_, _ = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "main reply")})

	w.SetHead(e1.ID) // rewind onto the fork
	forkA, _ := w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleUser, "fork prompt")})
	_, _ = w.Append(TypeMessage, MessageData{Message: llm.Text(llm.RoleAssistant, "fork reply")})
	w.SetHead(forkA.ID) // active head is the fork start
	require.NoError(t, w.Close())

	info, ok := readInfo(p)
	require.True(t, ok)

	// branch = root,e1,forkA: two messages (e1 + fork prompt), first from main.
	assert.Equal(t, 2, info.Messages)
	assert.Contains(t, info.First, "main prompt")
}

var _ = json.RawMessage(nil) // keep the import stable if helpers are pruned
