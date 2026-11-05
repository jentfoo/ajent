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
