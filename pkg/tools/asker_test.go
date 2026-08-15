package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGuardedToolExecuteWithAsker drives the ask path of Execute: an asker that
// allows lets the tool run, one that denies refuses with its reason, and one
// that re-asks is treated as a denial carrying the guard's original reason.
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
			asker:   func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) },
			wantRun: true,
		},
		{
			name:           "deny refuses with reason",
			asker:          func(_ context.Context, _ agent.ToolCall, _ Decision) Decision { return Deny("user said no") },
			wantDenyReason: "user said no",
		},
		{
			name:           "re-ask treated as denial",
			asker:          func(context.Context, agent.ToolCall, Decision) Decision { return Decision{Action: ActionAsk} },
			wantRun:        false,
			wantDenyReason: "needs approval", // original guard reason survives
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := New()
			inner := &recordingTool{}
			r.Register(inner, true)
			r.AddGuard(askGuard)
			r.SetAsker(tc.asker)

			tool, ok := r.Get("bash")
			require.True(t, ok)

			res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
			require.NoError(t, err) // a denial is a result, not an error
			if tc.wantRun {
				assert.False(t, res.IsError)
				assert.True(t, inner.ran)
				return
			}
			assert.True(t, res.IsError)
			assert.Contains(t, textOf(res), tc.wantDenyReason)
			assert.False(t, inner.ran) // nothing executed on a refusal
		})
	}
}

// TestAskerReceivesGuardDecision asserts the asker sees the call and the guard's
// Decision it is resolving.
func TestAskerReceivesGuardDecision(t *testing.T) {
	t.Parallel()

	var gotCall agent.ToolCall
	var gotDec Decision
	r := New()
	r.Register(&recordingTool{}, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "approval"}
	})
	r.SetAsker(func(_ context.Context, call agent.ToolCall, d Decision) Decision {
		gotCall = call
		gotDec = d
		return Allow(callWith(nil))
	})

	tool, _ := r.Get("bash")
	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	require.NoError(t, err)

	assert.False(t, res.IsError) // guard asked but asker overrode to allow
	assert.Equal(t, "c1", gotCall.ID)
	assert.Equal(t, ActionAsk, gotDec.Action)
	assert.Equal(t, "approval", gotDec.Reason)
}

// TestSetAskerNilRestoresDenial asserts clearing the asker makes an Ask refuse
// again exactly as when none was ever registered.
func TestSetAskerNilRestoresDenial(t *testing.T) {
	t.Parallel()

	r := New()
	inner := &recordingTool{}
	r.Register(inner, true)
	r.AddGuard(func(context.Context, agent.ToolCall) Decision {
		return Decision{Action: ActionAsk, Reason: "needs approval"}
	})
	r.SetAsker(func(context.Context, agent.ToolCall, Decision) Decision { return Allow(callWith(nil)) })
	r.SetAsker(nil)

	tool, _ := r.Get("bash")
	res, err := tool.Execute(t.Context(), callWith(json.RawMessage(`{}`)), nil)
	require.NoError(t, err)

	assert.True(t, res.IsError) // asker gone, so the Ask denies again
	assert.False(t, inner.ran)
}
