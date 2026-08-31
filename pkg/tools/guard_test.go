package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuardVerdict drives the guard chain over each decision: a denial returns an
// error result carrying its reason and never reaches the tool, a pass lets it run,
// and an Ask with no asker refuses like a denial.
func TestGuardVerdict(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		guard   Guard
		wantRun bool
		wantErr bool
		reason  string
	}{
		{
			name:    "deny_errors_and_skips_tool",
			guard:   func(context.Context, agent.ToolCall) Decision { return Deny("bash is blocked") },
			wantErr: true,
			reason:  "bash is blocked",
		},
		{
			name:    "allow_passes_through",
			guard:   func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) },
			wantRun: true,
		},
		{
			name: "ask_without_asker_denies",
			guard: func(context.Context, agent.ToolCall) Decision {
				return Decision{Action: ActionAsk, Reason: "needs approval"}
			},
			wantErr: true,
			reason:  "needs approval",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			inner := &recordingTool{}
			r.Register(inner, true)
			r.AddGuard(tc.guard)

			tool, ok := r.Get("bash")
			require.True(t, ok)

			res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
			require.NoError(t, err) // a denial is a result, not an error
			if tc.wantRun {
				assert.False(t, res.IsError)
				assert.True(t, inner.done)
				return
			}
			assert.True(t, res.IsError)
			assert.Contains(t, textOf(res), tc.reason)
			assert.False(t, inner.done) // nothing executed, so disk was untouched
		})
	}
}

func TestGuardChainFirstNonAllowWins(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) })
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Deny("second guard") })

	tool, ok := r.Get("bash")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "second guard")
}

func TestGuardDeniedCallLeavesFileOnDiskUntouched(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "keep.txt")
	require.NoError(t, os.WriteFile(path, []byte("original"), 0o644))

	r := New()
	r.Register(&fakeTool{name: "write"}, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Deny("no writes") })

	tool, ok := r.Get("write")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{"path":"`+path+`","content":"changed"}`)), nil)
	require.NoError(t, err)
	assert.True(t, res.IsError)

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "original", string(data)) // nothing on disk changed
}

// TestMustSerialize asserts the registry reports whether a batch would prompt: an
// Ask with an asker forces serial execution, while allow-only guards and a Deny
// (which resolves without prompting) do not. This is what keeps block-all's
// read-only dialogs in submission order.
func TestMustSerialize(t *testing.T) {
	t.Parallel()

	allow := func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) }
	ask := func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "needs approval"}
	}
	deny := func(_ context.Context, _ agent.ToolCall) Decision { return Deny("refused") }

	cases := []struct {
		name   string
		guards []Guard
		asker  Asker // nil when not set
		want   bool
	}{
		{"no guards", nil, func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) }, false},
		{"allow only with asker", []Guard{allow}, func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) }, false},
		{"ask without asker refuses not prompts", []Guard{ask}, nil, false},
		{"ask with asker serializes", []Guard{ask}, func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) }, true},
		{"deny resolves without prompting", []Guard{deny}, func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) }, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			r := New()
			r.Register(&recordingTool{}, true)
			for _, g := range c.guards {
				r.AddGuard(g)
			}
			if c.asker != nil {
				r.SetAsker(c.asker)
			}
			assert.Equal(t, c.want, r.MustSerialize([]agent.ToolCall{callWith(json.RawMessage(`{}`))}))
		})
	}
}

// TestMustSerializeAnyPromptingCall asserts a single Ask anywhere in the batch
// forces serial execution even when every other call is statically allowed.
func TestMustSerializeAnyPromptingCall(t *testing.T) {
	t.Parallel()

	r := New()
	r.Register(&recordingTool{}, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) })
	r.SetAsker(func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) })

	// a call whose guard asks must serialize the whole batch
	prompting := []agent.ToolCall{callWith(json.RawMessage(`{}`)), callWith(json.RawMessage(`{}`))}
	r.AddGuard(func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "needs approval"}
	})
	assert.True(t, r.MustSerialize(prompting))
}
