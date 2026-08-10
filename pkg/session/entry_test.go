package session

import (
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntryRoundTrip(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		typ  Type
		data any
	}{
		{"session", TypeSession, SessionData{Version: sessionVersion, Cwd: "/a", Workspace: "/w", Model: "p/id"}},
		{"message", TypeMessage, MessageData{
			Message: llm.Text(llm.RoleAssistant, "hi"),
			Stop:    llm.StopEndTurn,
			Usage:   llm.Usage{Input: 1, Output: 2},
		}},
		{"compaction", TypeCompaction, CompactionData{
			Summary: "summ", FirstKeptEntryID: "x", Before: 5, After: 3,
			Details: json.RawMessage(`{"k":1}`),
		}},
		{"model_change", TypeModelChange, ModelData{Model: "p/id", Reason: "manual"}},
		{"setting_change", TypeSettingChange, SettingData{
			Key: "reasoning", Value: json.RawMessage(`{"level":"medium"}`)},
		},
		{"notice", TypeNotice, NoticeData{Message: "m", Level: agent.LevelWarn}},
		{"custom", TypeCustom, CustomData{CustomType: "plan", Data: json.RawMessage(`{"s":1}`)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.data)
			require.NoError(t, err)
			e := Entry{ID: "id", Type: tc.typ, TS: 1, Data: data}
			b, err := json.Marshal(e)
			require.NoError(t, err)

			var back Entry
			require.NoError(t, json.Unmarshal(b, &back))
			assert.Equal(t, e.Type, back.Type)

			switch tc.typ {
			case TypeSession:
				var v SessionData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, tc.data.(SessionData), v)
			case TypeMessage:
				var v MessageData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, tc.data.(MessageData).Stop, v.Stop)
				assert.Equal(t, tc.data.(MessageData).Usage, v.Usage)
				assert.Equal(t, llm.RoleAssistant, v.Message.Role)
			case TypeCompaction:
				var v CompactionData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, tc.data.(CompactionData), v)
			case TypeModelChange:
				var v ModelData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, "p/id", v.Model)
			case TypeSettingChange:
				var v SettingData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, "reasoning", v.Key)
			case TypeNotice:
				var v NoticeData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, agent.LevelWarn, v.Level)
			case TypeCustom:
				var v CustomData
				require.NoError(t, back.Decode(&v))
				assert.Equal(t, "plan", v.CustomType)
			}
		})
	}
}

func TestEntryUnknownTypePreserved(t *testing.T) {
	t.Parallel()

	e := Entry{ID: "id", Type: "future_thing", TS: 1, Data: json.RawMessage(`{"a":1}`)}
	b, err := json.Marshal(e)
	require.NoError(t, err)

	var back Entry
	require.NoError(t, json.Unmarshal(b, &back))
	assert.Equal(t, Type("future_thing"), back.Type)
	assert.JSONEq(t, `{"a":1}`, string(back.Data))
}

func TestEntryUnknownFieldsIgnored(t *testing.T) {
	t.Parallel()

	b := []byte(`{
		"id":"x","parentId":"p","type":"message","ts":5,
		"data":{"message":{"role":"user","content":[{"type":"text","data":{"text":"hi"}}]},"stop":"end_turn","bogusField":99},
		"extraTop":1
	}`)
	var e Entry
	require.NoError(t, json.Unmarshal(b, &e))
	assert.Equal(t, "x", e.ID)

	var md MessageData
	require.NoError(t, e.Decode(&md))
	assert.Equal(t, llm.StopEndTurn, md.Stop)
}
