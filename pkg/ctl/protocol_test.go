package ctl

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDecodeIncomingSplitsRepliesFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("reply", func(t *testing.T) {
		in, err := DecodeIncoming([]byte(`{"id":"a1","seq":4,"result":{"head":"01J"}}`))
		require.NoError(t, err)
		require.NotNil(t, in.Reply)
		assert.Nil(t, in.Event)
		assert.Equal(t, "a1", in.Reply.ID)
		assert.Equal(t, uint64(4), in.Reply.Seq)
	})

	t.Run("event", func(t *testing.T) {
		in, err := DecodeIncoming([]byte(`{"event":"branch.changed","seq":5,"data":{"head":"01K"}}`))
		require.NoError(t, err)
		require.NotNil(t, in.Event)
		assert.Nil(t, in.Reply)
		assert.Equal(t, EventBranchChanged, in.Event.Name)
	})
}

func TestNewEventDropsUnencodableData(t *testing.T) {
	t.Parallel()

	ev := NewEvent(EventTextDelta, 1, map[string]any{"delta": make(chan int)})
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.JSONEq(t, `{"event":"text.delta","seq":1}`, string(raw))
}

func TestSubagentWireShapes(t *testing.T) {
	t.Parallel()

	t.Run("segment_counts_round_trip", func(t *testing.T) {
		raw, err := json.Marshal(Segment{Key: "subagents", Text: "2 running (oldest 31s), 1 queued",
			Order: 40, Counts: map[string]int{"running": 2, "queued": 1}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"key":"subagents","text":"2 running (oldest 31s), 1 queued",`+
			`"order":40,"counts":{"running":2,"queued":1}}`, string(raw))

		var seg Segment
		require.NoError(t, json.Unmarshal(raw, &seg))
		assert.Equal(t, 2, seg.Counts["running"])
		assert.Equal(t, 1, seg.Counts["queued"])
	})

	t.Run("nil_counts_omitted", func(t *testing.T) {
		raw, err := json.Marshal(Segment{Key: "mcp", Text: "2/2 servers"})
		require.NoError(t, err)
		assert.JSONEq(t, `{"key":"mcp","text":"2/2 servers","order":0}`, string(raw))
	})

	t.Run("agents_list_round_trip", func(t *testing.T) {
		raw, err := json.Marshal(AgentsListResult{Jobs: []AgentJob{
			{ID: "sub-1", Task: "look around", Status: "running", ElapsedMS: 31000},
			{ID: "sub-2", Status: "queued"},
		}})
		require.NoError(t, err)
		assert.JSONEq(t, `{"jobs":[`+
			`{"id":"sub-1","task":"look around","status":"running","elapsedMs":31000},`+
			`{"id":"sub-2","status":"queued"}]}`, string(raw))

		var res AgentsListResult
		require.NoError(t, json.Unmarshal(raw, &res))
		assert.Equal(t, "look around", res.Jobs[0].Task)
	})

	t.Run("agents_stop_params_round_trip", func(t *testing.T) {
		for _, tc := range []struct {
			params AgentsStopParams
			want   string
		}{
			{AgentsStopParams{ID: "sub-2"}, `{"id":"sub-2"}`},
			{AgentsStopParams{All: true}, `{"all":true}`},
		} {
			raw, err := json.Marshal(tc.params)
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(raw))
		}

		var res AgentsStopResult
		require.NoError(t, json.Unmarshal([]byte(`{"stopped":2}`), &res))
		assert.Equal(t, 2, res.Stopped)
	})
}
