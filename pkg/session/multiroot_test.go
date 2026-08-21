package session

import (
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBranchAcrossRoots covers the shape the plan workflow relies on: SetHead("")
// starts a second root whose branch stops there, so a rebuilt state carries only
// that root's own chain and resolves its model from the leading model_change.
func TestBranchAcrossRoots(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := filepath.Join(dir, "2026-01-02T03-04-05Z-f00d.jsonl")
	w, err := Create(p, SessionData{Version: sessionVersion, Model: "trunk-model"})
	require.NoError(t, err)
	r := NewRecorder(w)

	r.Message(msgInfo(llm.Text(llm.RoleUser, "prior chat")))
	r.Message(msgInfo(llm.Text(llm.RoleAssistant, "planning reply")))
	trunkTip := w.Head()

	// a fresh root: the model change lands first so State can resolve it
	w.SetHead("")
	r.ModelChange(llm.Model{Provider: "p", ID: "impl-model"}, "plan")
	require.NoError(t, r.Custom("plan", customPayload{Phase: "implementing", Round: 1}))
	r.Message(msgInfo(llm.Text(llm.RoleUser, "kickoff")))
	rootTip := w.Head()
	require.NoError(t, w.Sync())
	require.NoError(t, w.Close())

	entries := readEntries(t, p)

	implBranch := Branch(entries, rootTip)
	require.NotEmpty(t, implBranch)
	assert.Empty(t, implBranch[0].ParentID) // the branch really starts at a root

	st, warns := State(implBranch, resolveModel)
	assert.Empty(t, warns)
	assert.Equal(t, "p/impl-model", st.Model.ID) // resolveModel echoes the recorded key
	require.Len(t, st.Messages, 1)               // only the kickoff; the trunk is unreachable
	assert.Equal(t, "kickoff", textOf(st.Messages[0]))

	var got customPayload
	require.True(t, LatestCustom(implBranch, "plan", &got))
	assert.Equal(t, 1, got.Round)

	// the trunk is untouched and still rebuilds its own context
	trunk, trunkWarns := State(Branch(entries, trunkTip), resolveModel)
	assert.Empty(t, trunkWarns)
	assert.Len(t, trunk.Messages, 2)

	// the rewind picker renders both roots, marking only the live one active
	rows := TreeRows(entries, rootTip)
	active := make(map[string]bool, len(rows))
	for _, row := range rows {
		active[row.Label] = row.Active
	}
	require.Contains(t, active, "user: prior chat")
	require.Contains(t, active, "user: kickoff")
	assert.False(t, active["user: prior chat"])
	assert.True(t, active["user: kickoff"])
}

func msgInfo(m llm.Message) agent.MessageInfo {
	return agent.MessageInfo{Message: m, Stop: llm.StopEndTurn}
}
