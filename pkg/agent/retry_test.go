package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// retryableErr is a failure Recoverable accepts, standing in for an overloaded
// provider or a dropped connection.
func retryableErr() error {
	return &llm.APIError{Provider: "test", Message: "overloaded", Retryable: true}
}

// failTurn frames a mid-stream failure after the given text events streamed.
func failTurn(err error, streamed []llm.Event) llm.ScriptedTurn {
	return llm.ScriptedTurn{Events: append(streamed,
		llm.Event{Type: llm.EventDone, StopReason: llm.StopError, Err: err})}
}

// noWaitAgent replaces the retry backoff sleep with a no-op recording stub.
func noWaitAgent(a *Agent) *[]time.Duration {
	var delays []time.Duration
	a.retrySleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	return &delays
}

func TestStreamRetryRecovers(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		failTurn(retryableErr(), textEvents("par")),
		{Events: textOnly("done")},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)
	delays := noWaitAgent(a)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)
	assert.Len(t, *delays, 1) // one backoff before the retry
	assert.Positive(t, (*delays)[0])

	// the retry sends the identical request
	reqs := p.Requests()
	require.Len(t, reqs, 2)
	assert.Equal(t, reqs[0].Messages, reqs[1].Messages)

	// the partial attempt is closed before the notice, the answer follows
	var closed, notice, answered int
	for _, c := range sink.calls {
		switch {
		case c == "end_text":
			if notice == 0 {
				closed++
			}
		case strings.HasPrefix(c, "notice:stream failed"):
			notice++
		case c == "text" && notice > 0:
			answered++
		}
	}
	assert.Equal(t, 1, closed)
	assert.Equal(t, 1, notice)
	assert.Equal(t, 1, answered)
}

func TestStreamRetryClosesOpenThinking(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		failTurn(retryableErr(), []llm.Event{
			{Type: llm.EventThinkingStart, Index: 0},
			{Type: llm.EventThinkingDelta, Index: 0, Text: "hmm"},
		}),
		{Events: textOnly("done")},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)
	noWaitAgent(a)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err)

	// the unterminated thinking block is closed ahead of the retry
	endIdx, noticeIdx := -1, -1
	for i, c := range sink.calls {
		if c == "end_thinking" && endIdx < 0 {
			endIdx = i
		}
		if strings.HasPrefix(c, "notice:stream failed") && noticeIdx < 0 {
			noticeIdx = i
		}
	}
	require.NotEqual(t, -1, endIdx)
	require.NotEqual(t, -1, noticeIdx)
	assert.Less(t, endIdx, noticeIdx)
}

func TestStreamRetryExhausts(t *testing.T) {
	t.Parallel()

	// default retries (4) plus the first call: five identical failures
	turns := make([]llm.ScriptedTurn, 5)
	for i := range turns {
		turns[i] = llm.ScriptedTurn{Err: retryableErr()}
	}
	sentinel := retryableErr()
	turns[4] = llm.ScriptedTurn{Err: sentinel}

	p := &llm.ScriptedProvider{Turns: turns}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)
	noWaitAgent(a)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.Error(t, err)
	assert.Len(t, p.Requests(), 5)
	assert.Equal(t, sentinel, catch.result.Err)
}

func TestStreamRetrySkipsPermanent(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("wire failure")
	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		failTurn(sentinel, textEvents("par")),
		{Events: textOnly("never")},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.ErrorIs(t, err, sentinel)
	assert.Len(t, p.Requests(), 1) // a permanent error never re-requests
}

func TestStreamRetryInterruptInBackoff(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Err: retryableErr()},
		{Events: textOnly("never")},
	}}
	catch := &resultCatcher{}
	a := newTestAgent(nil, p, catch)
	a.retrySleep = func(ctx context.Context, _ time.Duration) error {
		a.Interrupt()
		return ctx.Err()
	}

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.NoError(t, err) // an interrupted retry is a clean abort
	assert.Equal(t, llm.StopAborted, catch.result.Stop)
}

func TestStreamRetryTurnRetriesOption(t *testing.T) {
	t.Parallel()

	p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
		{Err: retryableErr()},
		failTurn(retryableErr(), nil),
		{Events: textOnly("done")},
	}}
	sink := &recordingSink{}
	a := newTestAgent(nil, p, sink)
	a.opts.TurnRetries = 1
	noWaitAgent(a)

	err := a.Prompt(t.Context(), Input{Text: "x"})
	require.Error(t, err)          // budget 1: first call plus one retry
	assert.Len(t, p.Requests(), 2) // the third turn never runs
}
