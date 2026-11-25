package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

// gateAsk always allows, standing in for the asker every installed gate carries.
func gateAsk(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) }

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
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			inner := &recordingTool{}
			r.Register(inner, true)
			r.InstallGate([]Guard{tc.guard}, gateAsk)

			tool, ok := r.Get("bash")
			require.True(t, ok)

			res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
	r.InstallGate([]Guard{
		func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) },
		func(context.Context, agent.ToolCall) Decision { return Deny("second guard") },
	}, gateAsk)

	tool, ok := r.Get("bash")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), agent.NewOutput(agent.NopSink{}, "c"))
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
	r.InstallGate([]Guard{func(context.Context, agent.ToolCall) Decision { return Deny("no writes") }}, gateAsk)

	tool, ok := r.Get("write")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{"path":"`+path+`","content":"changed"}`)), agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)
	assert.True(t, res.IsError)

	data, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, "original", string(data)) // nothing on disk changed
}

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
		want   bool
	}{
		{"no_guards", nil, false},
		{"allow_only_with_asker", []Guard{allow}, false},
		{"ask_with_asker_serializes", []Guard{ask}, true},
		{"deny_resolves_without_prompting", []Guard{deny}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := New()
			r.Register(&recordingTool{}, true)
			r.InstallGate(c.guards, gateAsk)
			assert.Equal(t, c.want, r.MustSerialize([]agent.ToolCall{callWith(json.RawMessage(`{}`))}))
		})
	}

	// a call whose guard asks must serialize the whole batch
	t.Run("prompting_call_serializes_batch", func(t *testing.T) {
		r := New()
		r.Register(&recordingTool{}, true)
		r.InstallGate([]Guard{allow, ask}, gateAsk)

		prompting := []agent.ToolCall{callWith(json.RawMessage(`{}`)), callWith(json.RawMessage(`{}`))}
		assert.True(t, r.MustSerialize(prompting))
	})

	// a denial earlier in the batch must not mask a later call's ask: read-only
	// siblings would otherwise race ahead while an approval dialog is pending
	t.Run("later_ask_serializes_batch", func(t *testing.T) {
		r := New()
		r.Register(&recordingTool{}, true)
		r.InstallGate([]Guard{
			func(_ context.Context, c agent.ToolCall) Decision { // deny only the first call
				if string(c.Input) == `"first"` {
					return Deny("refused")
				}
				return Allow(callWith(nil))
			},
			ask,
		}, gateAsk)

		calls := []agent.ToolCall{
			callWith(json.RawMessage(`"first"`)),
			callWith(json.RawMessage(`{}`)),
		}
		assert.True(t, r.MustSerialize(calls)) // the second call still prompts
	})

	// the inverse: a denial resolves the call alone, no dialog for the ask behind it
	t.Run("denied_call_resolves_alone", func(t *testing.T) {
		r := New()
		r.Register(&recordingTool{}, true)
		r.InstallGate([]Guard{deny, ask}, gateAsk)
		assert.False(t, r.MustSerialize([]agent.ToolCall{callWith(json.RawMessage(`{}`))}))
	})
}
