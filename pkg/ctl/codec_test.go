package ctl

import (
	"bytes"
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

// TestDecoder pins Decode's line handling: whitespace between lines is
// tolerated, garbage is rejected.
func TestDecoder(t *testing.T) {
	t.Parallel()

	t.Run("skips_blank_lines", func(t *testing.T) {
		src := strings.NewReader("\n{\"id\":\"x\"}\n\n{\"id\":\"y\"}\n")
		dec := NewDecoder(src)
		var req Request
		require.NoError(t, dec.Decode(&req))
		assert.Equal(t, "x", req.ID)
		require.NoError(t, dec.Decode(&req))
		assert.Equal(t, "y", req.ID)
	})

	t.Run("tolerates_carriage_returns", func(t *testing.T) {
		dec := NewDecoder(strings.NewReader("{\"id\":\"x\"}\r\n"))
		var req Request
		require.NoError(t, dec.Decode(&req))
		assert.Equal(t, "x", req.ID)
	})

	t.Run("rejects_garbage", func(t *testing.T) {
		dec := NewDecoder(strings.NewReader("not json\n"))
		var req Request
		assert.Error(t, dec.Decode(&req))
	})
}
