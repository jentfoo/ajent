package agent

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoverTool(t *testing.T) {
	t.Parallel()

	call := ToolCall{ID: "c1", Name: "explode", Input: json.RawMessage(`{}`)}

	t.Run("no_panic_passthrough", func(t *testing.T) {
		res, err, faulted := RecoverTool(t.Context(), nil, nil, call, func() (ToolResult, error) {
			return ToolResult{Content: llm.BlockList{llm.TextBlock{Text: "fine"}}}, nil
		})
		assert.False(t, faulted)
		assert.Equal(t, "fine", textOf(res.Content))
		assert.False(t, res.IsError)
		assert.NoError(t, err)
	})

	t.Run("no_panic_error_passthrough", func(t *testing.T) {
		sent := errors.New("tool said no")
		res, err, faulted := RecoverTool(t.Context(), nil, nil, call, func() (ToolResult, error) {
			return ToolResult{}, sent
		})
		assert.False(t, faulted)
		assert.False(t, res.IsError)
		assert.Equal(t, sent, err)
	})

	t.Run("panic_becomes_error_result", func(t *testing.T) {
		sink := &recordingSink{}
		res, err, faulted := RecoverTool(t.Context(), nil, sink, call, func() (ToolResult, error) {
			panic("kaboom")
		})
		assert.True(t, faulted)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res.Content), "tool panicked: kaboom")
		require.NoError(t, err)

		notices := bulk.SliceFilter(func(c string) bool {
			return strings.HasPrefix(c, "notice:")
		}, sink.calls)
		require.Len(t, notices, 1)
		assert.Contains(t, notices[0], "kaboom")
		assert.Contains(t, notices[0], "TestRecoverTool")
	})

	t.Run("panic_error_value", func(t *testing.T) {
		res, _, faulted := RecoverTool(t.Context(), nil, nil, call, func() (ToolResult, error) {
			panic(errors.New("err panic"))
		})
		assert.True(t, faulted)
		assert.Contains(t, textOf(res.Content), "err panic")
	})

	t.Run("detail_clipped", func(t *testing.T) {
		res, _, faulted := RecoverTool(t.Context(), nil, nil, call, func() (ToolResult, error) {
			panic(strings.Repeat("x", maxFaultDetail+50))
		})
		assert.True(t, faulted)
		text := textOf(res.Content)
		assert.Len(t, text, len("tool panicked: ")+maxFaultDetail+2) // Clip appends an ellipsis rune
		assert.True(t, strings.HasSuffix(text, "…"))
	})

	t.Run("nil_sink_skips_notice", func(t *testing.T) {
		res, _, faulted := RecoverTool(t.Context(), nil, nil, call, func() (ToolResult, error) {
			panic("alone")
		})
		assert.True(t, faulted)
		assert.True(t, res.IsError)
	})

	t.Run("cancelled_context_skips_notice", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sink := &recordingSink{}
		res, _, faulted := RecoverTool(ctx, nil, sink, call, func() (ToolResult, error) {
			panic("late")
		})
		assert.True(t, faulted)
		assert.True(t, res.IsError)
		assert.Empty(t, sink.calls)
	})

	t.Run("goexit_not_recovered", func(t *testing.T) {
		// Goexit ends the calling goroutine, so run it on a throwaway one and
		// observe the effects from here
		var mu sync.Mutex
		var ended bool
		var recovered bool
		go func() {
			defer func() {
				if p := recover(); p != nil {
					mu.Lock()
					recovered = true
					mu.Unlock()
				}
				mu.Lock()
				ended = true
				mu.Unlock()
			}()
			_, _, _ = RecoverTool(t.Context(), nil, nil, call, func() (ToolResult, error) {
				runtime.Goexit()
				return ToolResult{}, nil // unreachable, keeps vet happy
			})
		}()
		require.Eventually(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return ended
		}, time.Second, time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		assert.False(t, recovered) // Goexit was preserved, not converted
	})
}

func TestFaultLocation(t *testing.T) {
	t.Parallel()

	t.Run("names_faulting_frame", func(t *testing.T) {
		_, _, faulted := RecoverTool(t.Context(), nil, nil, ToolCall{}, func() (ToolResult, error) {
			panic("locate me")
		})
		assert.True(t, faulted)
		loc := faultLocation()
		assert.NotEmpty(t, loc)
		assert.Contains(t, loc, "TestFaultLocation")
	})
}
