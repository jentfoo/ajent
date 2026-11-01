package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
)

func TestGuardedToolExecuteWithAsker(t *testing.T) {
	t.Parallel()

	askGuard := func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "needs approval"}
	}

	cases := []struct {
		name           string
		asker          Asker
		wantRun        bool
		wantDenyReason string
	}{
		{
			name:    "allow lets tool run",
			asker:   func(context.Context, agent.ToolCall) Decision { return Allow(callWith(nil)) },
			wantRun: true,
		},
		{
			name:           "deny refuses with reason",
			asker:          func(_ context.Context, _ agent.ToolCall) Decision { return Deny("user said no") },
			wantDenyReason: "user said no",
		},
		{
			name:           "re-ask treated as denial",
			asker:          func(context.Context, agent.ToolCall) Decision { return Decision{Action: ActionAsk} },
			wantRun:        false,
			wantDenyReason: "needs approval", // original guard reason survives
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			inner := &recordingTool{}
			r.Register(inner, true)
			r.InstallGate([]Guard{askGuard}, tc.asker)

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
			assert.Contains(t, textOf(res), tc.wantDenyReason)
			assert.False(t, inner.done) // nothing executed on a refusal
		})
	}
}

func TestAskerReceivesCall(t *testing.T) {
	t.Parallel()

	var gotCall agent.ToolCall
	r := New()
	r.Register(&recordingTool{}, true)
	r.InstallGate([]Guard{func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "approval"}
	}}, func(_ context.Context, call agent.ToolCall) Decision {
		gotCall = call
		return Allow(callWith(nil))
	})

	tool, ok := r.Get("bash")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)

	assert.False(t, res.IsError) // guard asked but asker overrode to allow
	assert.Equal(t, "c1", gotCall.ID)
}

func TestInstallGateReplacesPrevious(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	ask := func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "needs approval"}
	}
	r.InstallGate([]Guard{ask}, func(context.Context, agent.ToolCall) Decision { return Deny("first asker") })
	r.InstallGate([]Guard{ask}, func(_ context.Context, _ agent.ToolCall) Decision { return Allow(callWith(nil)) })

	tool, ok := r.Get("bash")
	require.True(t, ok)

	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), agent.NewOutput(agent.NopSink{}, "c"))
	require.NoError(t, err)

	assert.False(t, res.IsError) // the replacement asker decided, not the first
	assert.True(t, inner.done)
}
