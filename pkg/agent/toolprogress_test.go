package agent

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolProgress(t *testing.T) {
	t.Parallel()

	t.Run("start_opens_an_empty_row", func(t *testing.T) {
		var p toolProgress
		got := p.start("c1", 0, "write")
		assert.Equal(t, ToolProgress{CallID: "c1", Name: "write"}, got)
	})
	t.Run("delta_below_step_publishes_nothing", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		_, ok := p.delta("c1", 0, `{"path":"a.go","content":"x"}`)
		assert.False(t, ok) // under progressStep, the row would repaint per token
	})
	t.Run("delta_publishes_once_past_the_step", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		_, ok := p.delta("c1", 0, `{"path":"a.go","content":"`+strings.Repeat("x", progressStep)+`"`)
		require.True(t, ok)

		got, ok := p.end("c1", 0)
		require.True(t, ok)
		assert.Equal(t, "write", got.Name)
		assert.Equal(t, "a.go", got.Path)
		assert.True(t, got.Done)
		assert.Greater(t, got.Bytes, progressStep)
	})
	t.Run("counts_newline_escapes_across_chunks", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		// the escape is split across the chunk boundary
		p.delta("c1", 0, `{"content":"a\`)
		p.delta("c1", 0, `nb\nc"}`)
		got, ok := p.end("c1", 0)
		require.True(t, ok)
		assert.Equal(t, 2, got.Lines)
	})
	t.Run("escaped_backslash_is_not_a_newline", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		p.delta("c1", 0, `{"content":"a\\nb"}`) // literal backslash then n
		got, ok := p.end("c1", 0)
		require.True(t, ok)
		assert.Zero(t, got.Lines)
	})
	t.Run("pairs_by_index_when_id_is_absent", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		// providers repeat the id on some events but always carry the block index
		p.delta("", 0, `{"path":"a.go","content":"x\ny"}`)
		got, ok := p.end("", 0)
		require.True(t, ok)
		assert.Equal(t, "c1", got.CallID)
		assert.Equal(t, "a.go", got.Path)
		assert.Equal(t, 1, got.Lines)
	})
	t.Run("keeps_parallel_calls_apart", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		p.start("c2", 1, "edit")
		p.delta("", 0, `{"path":"a.go","content":"x\n"}`)
		p.delta("", 1, `{"path":"b.go"}`)

		a, ok := p.end("", 0)
		require.True(t, ok)
		b, ok := p.end("", 1)
		require.True(t, ok)
		assert.Equal(t, "a.go", a.Path)
		assert.Equal(t, 1, a.Lines)
		assert.Equal(t, "b.go", b.Path)
		assert.Zero(t, b.Lines)
	})
	t.Run("unknown_call_is_ignored", func(t *testing.T) {
		var p toolProgress
		_, ok := p.delta("nope", 9, "{}")
		assert.False(t, ok)
		_, ok = p.end("nope", 9)
		assert.False(t, ok)
	})
	t.Run("clear_closes_every_in_flight_call", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		p.start("c2", 1, "edit")
		got := p.clear()
		require.Len(t, got, 2)
		for _, g := range got {
			assert.True(t, g.Done)
		}
		assert.Empty(t, p.clear()) // and forgets them
	})
	t.Run("end_forgets_the_call", func(t *testing.T) {
		var p toolProgress
		p.start("c1", 0, "write")
		_, ok := p.end("c1", 0)
		require.True(t, ok)
		_, ok = p.end("c1", 0)
		assert.False(t, ok)
	})
}

func TestArgTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"complete_pair", `{"path":"a.go","content":"x"}`, "a.go"},
		{"spaced_colon", `{"path" : "a.go"`, "a.go"},
		// a marshalled Go map sorts its keys, so content streams ahead of path
		{"path_after_long_content", `{"content":"aaaa`, ""},
		{"path_found_after_content", `{"content":"aaa","path":"a.go"}`, "a.go"},
		{"value_still_streaming", `{"path":"a.g`, ""},
		{"key_still_streaming", `{"pa`, ""},
		{"empty_input", ``, ""},
		{"no_colon_yet", `{"path"`, ""},
		{"non_string_value", `{"path":12}`, ""},
		{"escaped_quote_in_value", `{"path":"a\"b"}`, `a\"b`},
		{"empty_value", `{"path":""}`, ""},
		{"falls_back_to_pattern", `{"pattern":"TODO","glob":"*.go"}`, "TODO"},
		{"falls_back_to_command", `{"command":"go test ./..."}`, "go test ./..."},
		{"unknown_keys_only", `{"depth":2,"other":"x"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, argTarget(tc.input))
		})
	}
}
