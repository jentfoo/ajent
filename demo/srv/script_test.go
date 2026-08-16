package srv

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		roles []string
		want  int
	}{
		{"fresh_context", nil, 0},
		{"one_turn_in", []string{"system", "user", "assistant", "tool"}, 1},
		{"two_turns_in", []string{"system", "user", "assistant", "tool", "assistant", "tool"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var msgs []chatMessage
			for _, r := range tc.roles {
				msgs = append(msgs, chatMessage{Role: r})
			}
			assert.Equal(t, tc.want, stepIndex(chatRequest{Messages: msgs}))
		})
	}
}

func TestRecoverRun(t *testing.T) {
	t.Parallel()

	ts := int64(1712345678901234567)
	r, ok := recoverRun(chatRequest{Messages: []chatMessage{{
		Role: "assistant",
		ToolCalls: []assistantCall{{Function: callInvocation{
			Name:      "bash",
			Arguments: jsonArgs(map[string]any{"command": fmt.Sprintf("mkdir -p %s%d", scratchPrefix, ts)}),
		}}},
	}}})
	require.True(t, ok)
	assert.Equal(t, fmt.Sprintf("%s%d", scratchPrefix, ts), r.dir)

	t.Run("no_bash_call_yet", func(t *testing.T) {
		_, ok := recoverRun(chatRequest{Messages: nil})
		assert.False(t, ok)
	})

	t.Run("non_bash_tool_ignored", func(t *testing.T) {
		_, ok := recoverRun(chatRequest{Messages: []chatMessage{{
			Role: "assistant",
			ToolCalls: []assistantCall{{Function: callInvocation{
				Name:      "write",
				Arguments: jsonArgs(map[string]any{"path": "/tmp/other", "content": "x"}),
			}}},
		}}})
		assert.False(t, ok)
	})

	t.Run("string_arguments_decoded", func(t *testing.T) {
		r, ok := recoverRun(chatRequest{Messages: []chatMessage{{
			Role: "assistant",
			ToolCalls: []assistantCall{{Function: callInvocation{
				Name:      "bash",
				Arguments: jsonArgs(fmt.Sprintf("mkdir -p %s%d", scratchPrefix, ts)),
			}}},
		}}})
		require.True(t, ok)
		assert.Equal(t, fmt.Sprintf("%s%d", scratchPrefix, ts), r.dir)
	})
}

func TestResolveCall(t *testing.T) {
	t.Parallel()

	r := &run{dir: "/tmp/ajent-demo-1"}
	writeCall := stepCall{name: "write", bash: "cat > <dir>/notes.go",
		args: func(r *run) []byte { return jsonArgs(map[string]any{"path": r.dir + "/notes.go"}) }}

	t.Run("native_name_matches", func(t *testing.T) {
		name, args, ok := resolveCall([]string{"read", "write"}, writeCall, r)
		require.True(t, ok)
		assert.Equal(t, "write", name)
		assert.Contains(t, string(args), "/tmp/ajent-demo-1/notes.go")
	})

	t.Run("case_insensitive_match", func(t *testing.T) {
		name, _, ok := resolveCall([]string{"Write"}, writeCall, r)
		require.True(t, ok)
		assert.Equal(t, "write", name)
	})

	t.Run("falls_back_to_bash", func(t *testing.T) {
		name, args, ok := resolveCall([]string{"bash"}, writeCall, r)
		require.True(t, ok)
		assert.Equal(t, "bash", name)
		var got map[string]any
		require.NoError(t, json.Unmarshal(args, &got))
		assert.Contains(t, got["command"], "/tmp/ajent-demo-1/notes.go")
	})

	t.Run("advertises_nothing", func(t *testing.T) {
		name, _, ok := resolveCall(nil, writeCall, r)
		assert.False(t, ok)
		assert.Empty(t, name)
	})
}

func TestBashName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{"read", "bash"}, "bash"},
		{[]string{"run_shell_cmd"}, "run_shell_cmd"},
		{nil, ""},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, bashName(tc.in))
	}
}

// TestScriptSteps locks the demo's tool choreography to spec: every step but
// the last must emit at least one call, and only the final step ends with none.
func TestScriptSteps(t *testing.T) {
	t.Parallel()
	stps := script()
	require.NotEmpty(t, stps)
	for i, stp := range stps {
		if i < len(stps)-1 {
			assert.NotEmptyf(t, stp.calls, "step %d must carry a tool call", i)
		}
	}
	// the closing step ends the turn with prose only
	last := stps[len(stps)-1]
	assert.Empty(t, last.calls)
	require.NotEmpty(t, last.think) // wrap-up thinking rides the final message
}

func TestAdvertisedNames(t *testing.T) {
	t.Parallel()
	req := chatRequest{Tools: []toolSpec{
		{Function: toolFunction{Name: "read"}},
		{Function: toolFunction{Name: "  bash "}},
	}}
	assert.Equal(t, []string{"read", "bash"}, advertisedNames(req))
}
