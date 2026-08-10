package session

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolveModel(key string) (llm.Model, error) {
	return llm.Model{Provider: "p", ID: key}, nil
}

func TestStateRebuildAppliesInOrder(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		modelChange("one"),
		settingReasoning(`{"level":"medium"}`),
		msgWithID("m1", llm.Text(llm.RoleUser, "hi")),
		msgWithID("m2", llm.Text(llm.RoleAssistant, "yo")),
	}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Equal(t, llm.Model{Provider: "p", ID: "one"}, st.Model)
	testLevel, ok := llm.ParseLevel("medium")
	require.True(t, ok)
	assert.Equal(t, testLevel, st.Reasoning.Level)
	require.Len(t, st.Messages, 2)
	assert.Equal(t, llm.RoleUser, st.Messages[0].Role)
}

func TestStateAppliesToolsSetting(t *testing.T) {
	t.Parallel()

	branch := []Entry{settingChange("tools", `["read","bash"]`)}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Equal(t, []string{"read", "bash"}, st.Tools)
}

func TestStateCompactionEmitsSummaryAndKeptTail(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		msgWithID("m1", llm.Text(llm.RoleUser, "first")),
		msgWithID("m2", llm.Text(llm.RoleAssistant, "second")),
		{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"summed","firstKeptEntryId":"m3"}`)},
		msgWithID("m3", llm.Text(llm.RoleUser, "kept start")),
		msgWithID("m4", llm.Text(llm.RoleAssistant, "kept end")),
	}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	require.Len(t, st.Messages, 3) // summary + m3 + m4
	sys := st.Messages[0]
	assert.Equal(t, llm.RoleSystem, sys.Role)
	assert.Contains(t, textOf(sys), "summed")
	assert.Equal(t, "kept start", textOf(st.Messages[1]))
}

func TestStateCompactionKeepsFromEarlierMessage(t *testing.T) {
	t.Parallel()

	// firstKeptEntryId points before the compaction entry in file order.
	branch := []Entry{
		msgWithID("m1", llm.Text(llm.RoleUser, "drop me")),
		msgWithID("m2", llm.Text(llm.RoleAssistant, "keep me")),
		{ID: "comp", Type: TypeCompaction, Data: compactData(`{"summary":"s","firstKeptEntryId":"m2"}`)},
	}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	require.Len(t, st.Messages, 2)
	assert.Equal(t, "keep me", textOf(st.Messages[1]))
}

func TestStateUnresolvableModelWarns(t *testing.T) {
	t.Parallel()

	branch := []Entry{
		modelChange("ghost"), // resolve fails on this key
		msgWithID("m1", llm.Text(llm.RoleUser, "hi")),
	}
	resolve := func(key string) (llm.Model, error) {
		return llm.Model{}, fmt.Errorf("no such model %s", key)
	}
	st, warns := State(branch, resolve)
	require.Len(t, st.Messages, 1) // the message still rebuilds
	assert.NotEmpty(t, warns)
}

func TestStateWarnsOnDanglingToolCall(t *testing.T) {
	t.Parallel()

	assistantWithCall := MessageData{Message: llm.Message{Role: llm.RoleAssistant,
		Content: llm.BlockList{llm.ToolCallBlock{ID: "c1", Name: "bash"}}}}
	b, _ := json.Marshal(assistantWithCall)
	st, warns := State([]Entry{{ID: "a1", Type: TypeMessage, Data: b}}, resolveModel)
	require.Len(t, st.Messages, 1) // the tool-call message rebuilds
	_ = st
	assert.NotEmpty(t, warns)
}

func TestStateUnknownSettingIgnored(t *testing.T) {
	t.Parallel()

	branch := []Entry{settingChange("bogus", `1`)}
	st, warns := State(branch, resolveModel)
	assert.Empty(t, warns)
	assert.Nil(t, st.Tools)
}

// helpers ---------------------------------------------------------

func modelChange(key string) Entry { return entry(TypeModelChange, ModelData{Model: key}) }

func settingReasoning(v string) Entry {
	return entry(TypeSettingChange, SettingData{Key: "reasoning", Value: json.RawMessage(v)})
}

func settingChange(key, v string) Entry {
	return entry(TypeSettingChange, SettingData{Key: key, Value: json.RawMessage(v)})
}

func msgWithID(id string, m llm.Message) Entry {
	return Entry{ID: id, Type: TypeMessage, Data: mustJSON(MessageData{Message: m})}
}

func compactData(s string) []byte { return json.RawMessage(s) }

func entry(tp Type, data any) Entry {
	return Entry{Type: tp, Data: mustJSON(data)}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func textOf(m llm.Message) string {
	for _, blk := range m.Content {
		if tb, ok := blk.(llm.TextBlock); ok {
			return tb.Text
		}
	}
	return ""
}
