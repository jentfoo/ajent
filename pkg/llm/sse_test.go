package llm

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// drainFrames reads every frame until the stream ends, returning the terminal error.
func drainFrames(t *testing.T, r *SSEReader) ([]Frame, error) {
	t.Helper()

	var out []Frame
	for {
		f, err := r.Next(t.Context())
		if err != nil {
			return out, err
		}
		out = append(out, f)
	}
}

func TestSSEReaderNext(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected []Frame
	}{
		{
			"single_data_line",
			"data: hello\n\n",
			[]Frame{{Data: []byte("hello")}},
		},
		{
			"multi_line_data_joined",
			"data: one\ndata: two\ndata: three\n\n",
			[]Frame{{Data: []byte("one\ntwo\nthree")}},
		},
		{
			"event_and_data",
			"event: message_start\ndata: {\"a\":1}\n\n",
			[]Frame{{Event: "message_start", Data: []byte(`{"a":1}`)}},
		},
		{
			"crlf_line_endings",
			"event: ping\r\ndata: x\r\n\r\n",
			[]Frame{{Event: "ping", Data: []byte("x")}},
		},
		{
			"bare_cr_line_endings",
			"data: x\r\r",
			[]Frame{{Data: []byte("x")}},
		},
		{
			"comment_only_block_skipped",
			": heartbeat\n\ndata: after\n\n",
			[]Frame{{Data: []byte("after")}},
		},
		{
			"comments_between_fields",
			"event: a\n: keepalive\ndata: b\n\n",
			[]Frame{{Event: "a", Data: []byte("b")}},
		},
		{
			"event_without_data_does_not_dispatch",
			"event: lonely\n\ndata: real\n\n",
			[]Frame{{Data: []byte("real")}},
		},
		{
			"id_carries_forward",
			"id: 1\ndata: a\n\ndata: b\n\n",
			[]Frame{{ID: "1", Data: []byte("a")}, {ID: "1", Data: []byte("b")}},
		},
		{
			"retry_parsed",
			"retry: 2500\ndata: a\n\n",
			[]Frame{{Retry: 2500 * time.Millisecond, Data: []byte("a")}},
		},
		{
			"field_without_space",
			"data:tight\n\n",
			[]Frame{{Data: []byte("tight")}},
		},
		{
			"only_one_leading_space_stripped",
			"data:  padded\n\n",
			[]Frame{{Data: []byte(" padded")}},
		},
		{
			"empty_data_value",
			"data:\n\n",
			[]Frame{{Data: []byte("")}},
		},
		{
			"unknown_field_ignored",
			"bogus: x\ndata: a\n\n",
			[]Frame{{Data: []byte("a")}},
		},
		{
			"colonless_line_ignored",
			"justtext\ndata: a\n\n",
			[]Frame{{Data: []byte("a")}},
		},
		{
			"final_frame_without_trailing_blank",
			"data: a\n\ndata: b\n",
			[]Frame{{Data: []byte("a")}},
		},
		{
			"done_sentinel_is_a_frame",
			"data: [DONE]\n\n",
			[]Frame{{Data: []byte("[DONE]")}},
		},
		{
			"multiple_frames",
			"data: a\n\ndata: b\n\ndata: c\n\n",
			[]Frame{{Data: []byte("a")}, {Data: []byte("b")}, {Data: []byte("c")}},
		},
		{
			"empty_stream",
			"",
			nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := drainFrames(t, NewSSEReader(strings.NewReader(tc.input), 0))
			require.ErrorIs(t, err, io.EOF)
			assert.Equal(t, tc.expected, got)
		})
	}
}

func TestSSEReaderIsDone(t *testing.T) {
	t.Parallel()

	r := NewSSEReader(strings.NewReader("data: hi\n\ndata: [DONE]\n\n"), 0)
	first, err := r.Next(t.Context())
	require.NoError(t, err)
	assert.False(t, first.IsDone())

	second, err := r.Next(t.Context())
	require.NoError(t, err)
	assert.True(t, second.IsDone())
}

func TestSSEReaderLastEventID(t *testing.T) {
	t.Parallel()

	t.Run("tracks_latest", func(t *testing.T) {
		r := NewSSEReader(strings.NewReader("id: 1\ndata: a\n\nid: 2\ndata: b\n\n"), 0)
		_, err := r.Next(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "1", r.LastEventID())

		_, err = r.Next(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "2", r.LastEventID())
	})
	t.Run("nul_in_id_ignored", func(t *testing.T) {
		r := NewSSEReader(strings.NewReader("id: a\x00b\ndata: x\n\n"), 0)
		_, err := r.Next(t.Context())
		require.NoError(t, err)
		assert.Empty(t, r.LastEventID())
	})
}

func TestSSEReaderFrameBound(t *testing.T) {
	t.Parallel()

	// the bound covers the whole line, so the "data: " prefix counts against it
	t.Run("line_at_the_bound_is_allowed", func(t *testing.T) {
		body := "data: " + strings.Repeat("x", 58) + "\n\n"
		f, err := NewSSEReader(strings.NewReader(body), 64).Next(t.Context())
		require.NoError(t, err)
		assert.Len(t, f.Data, 58)
	})
	t.Run("line_one_over_the_bound_fails", func(t *testing.T) {
		body := "data: " + strings.Repeat("x", 59) + "\n\n"
		_, err := NewSSEReader(strings.NewReader(body), 64).Next(t.Context())
		assert.ErrorIs(t, err, ErrFrameTooLarge)
	})
	t.Run("unterminated_stream_fails", func(t *testing.T) {
		_, err := NewSSEReader(strings.NewReader(strings.Repeat("x", 500)), 64).Next(t.Context())
		assert.ErrorIs(t, err, ErrFrameTooLarge)
	})
	t.Run("accumulated_lines_hit_the_bound", func(t *testing.T) {
		body := strings.Repeat("data: "+strings.Repeat("x", 20)+"\n", 10) + "\n"
		_, err := NewSSEReader(strings.NewReader(body), 64).Next(t.Context())
		assert.ErrorIs(t, err, ErrFrameTooLarge)
	})
	t.Run("error_is_latched", func(t *testing.T) {
		body := "data: " + strings.Repeat("x", 65) + "\n\ndata: fine\n\n"
		r := NewSSEReader(strings.NewReader(body), 64)
		_, err := r.Next(t.Context())
		require.ErrorIs(t, err, ErrFrameTooLarge)

		_, err = r.Next(t.Context())
		assert.ErrorIs(t, err, ErrFrameTooLarge) // does not resync on the rest
	})
}

func TestSSEReaderCancellation(t *testing.T) {
	t.Parallel()

	t.Run("cancelled_before_read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_, err := NewSSEReader(strings.NewReader("data: a\n\n"), 0).Next(ctx)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("cancelled_mid_frame", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pr.Close() })

		ctx, cancel := context.WithCancel(t.Context())
		r := NewSSEReader(pr, 0)

		// the write blocks until Next consumes it, so Next is mid frame with no
		// blank line to dispatch on when the cancellation lands
		go func() {
			_, _ = pw.Write([]byte("data: partial\n"))
			cancel()
			_ = pw.CloseWithError(context.Canceled)
		}()

		_, err := r.Next(ctx)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestSSEReaderClose(t *testing.T) {
	t.Parallel()

	t.Run("closes_underlying_closer", func(t *testing.T) {
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })

		r := NewSSEReader(pr, 0)
		require.NoError(t, r.Close())

		_, err := r.Next(t.Context())
		assert.ErrorIs(t, err, io.EOF)
	})
	t.Run("plain_reader_is_fine", func(t *testing.T) {
		r := NewSSEReader(strings.NewReader("data: a\n\n"), 0)
		assert.NoError(t, r.Close())
	})
}
