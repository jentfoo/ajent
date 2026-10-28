package ctl

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodecRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	require.NoError(t, enc.Encode(Request{ID: "a1", Method: MethodState}))
	require.NoError(t, enc.Encode(NewEvent(EventTextDelta, 7, TextData{Delta: "hi"})))

	dec := NewDecoder(&buf)
	var req Request
	require.NoError(t, dec.Decode(&req))
	assert.Equal(t, "a1", req.ID)
	assert.Equal(t, MethodState, req.Method)

	var ev Event
	require.NoError(t, dec.Decode(&ev))
	assert.Equal(t, EventTextDelta, ev.Name)
	assert.Equal(t, uint64(7), ev.Seq)
	var td TextData
	require.NoError(t, ev.Decode(&td))
	assert.Equal(t, "hi", td.Delta)

	assert.ErrorIs(t, dec.Decode(&req), io.EOF)
}

func TestDecoderSkipsBlankLines(t *testing.T) {
	t.Parallel()

	src := strings.NewReader("\n{\"id\":\"x\"}\n\n{\"id\":\"y\"}\n")
	dec := NewDecoder(src)
	var req Request
	require.NoError(t, dec.Decode(&req))
	assert.Equal(t, "x", req.ID)
	require.NoError(t, dec.Decode(&req))
	assert.Equal(t, "y", req.ID)
}

func TestDecoderToleratesCarriageReturns(t *testing.T) {
	t.Parallel()

	dec := NewDecoder(strings.NewReader("{\"id\":\"x\"}\r\n"))
	var req Request
	require.NoError(t, dec.Decode(&req))
	assert.Equal(t, "x", req.ID)
}

func TestDecoderRejectsGarbage(t *testing.T) {
	t.Parallel()

	dec := NewDecoder(strings.NewReader("not json\n"))
	var req Request
	assert.Error(t, dec.Decode(&req))
}

func TestDecodeIncomingSplitsRepliesFromEvents(t *testing.T) {
	t.Parallel()

	in, err := DecodeIncoming([]byte(`{"id":"a1","seq":4,"result":{"head":"01J"}}`))
	require.NoError(t, err)
	require.NotNil(t, in.Reply)
	assert.Nil(t, in.Event)
	assert.Equal(t, "a1", in.Reply.ID)
	assert.Equal(t, uint64(4), in.Reply.Seq)

	in, err = DecodeIncoming([]byte(`{"event":"head","seq":5,"data":{"id":"01K"}}`))
	require.NoError(t, err)
	require.NotNil(t, in.Event)
	assert.Nil(t, in.Reply)
	assert.Equal(t, EventHead, in.Event.Name)
}

func TestNewEventDropsUnencodableData(t *testing.T) {
	t.Parallel()

	ev := NewEvent(EventTextDelta, 1, map[string]any{"delta": make(chan int)})
	raw, err := json.Marshal(ev)
	require.NoError(t, err)
	assert.JSONEq(t, `{"event":"text.delta","seq":1}`, string(raw))
}
