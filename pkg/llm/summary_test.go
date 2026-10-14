package llm

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedStreamProvider serves one pre-built Stream, so a test controls both the
// events and the error a drained stream reports.
type fixedStreamProvider struct{ st Stream }

func (p *fixedStreamProvider) Name() string { return "fixed" }

func (p *fixedStreamProvider) Stream(context.Context, Request) (Stream, error) {
	return p.st, nil
}

func TestRunSummary(t *testing.T) {
	t.Parallel()

	usage := Usage{Input: 900, Output: 30}
	summary := []Event{
		{Type: EventUsage, Usage: usage},
		{Type: EventTextStart, Index: 0},
		{Type: EventTextDelta, Index: 0, Text: "## Goal"},
		{Type: EventTextEnd, Index: 0, Block: TextBlock{Text: "## Goal"}},
	}
	drainErr := errors.New("connection reset")
	doneErr := errors.New("overloaded")

	cases := []struct {
		name   string
		stream Stream
		want   Usage
		err    error
	}{
		{
			name:   "success_returns_text_and_usage",
			stream: &SliceStream{Events: slices.Clone(summary)},
			want:   usage,
		},
		{
			name:   "stream_error_keeps_usage",
			stream: &SliceStream{Events: slices.Clone(summary), Error: drainErr},
			want:   usage,
			err:    drainErr,
		},
		{
			name: "done_error_keeps_usage",
			stream: &SliceStream{Events: append(slices.Clone(summary),
				Event{Type: EventDone, StopReason: StopError, Err: doneErr})},
			want: usage,
			err:  doneErr,
		},
		{
			name: "max_tokens_keeps_usage",
			stream: &SliceStream{Events: append(slices.Clone(summary),
				Event{Type: EventDone, StopReason: StopMaxTokens})},
			want: usage,
			err:  ErrTruncated,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, got, err := RunSummary(t.Context(), &fixedStreamProvider{st: tc.stream}, Request{})
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Empty(t, text) // never partial text
			} else {
				require.NoError(t, err)
				assert.Equal(t, "## Goal", text)
			}
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("open_error_has_no_usage", func(t *testing.T) {
		p := &ScriptedProvider{Turns: []ScriptedTurn{{Err: errors.New("no route")}}}
		text, got, err := RunSummary(t.Context(), p, Request{})
		require.Error(t, err)
		assert.Empty(t, text)
		assert.Zero(t, got) // nothing streamed, so nothing was billed
	})
}
