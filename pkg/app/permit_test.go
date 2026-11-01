package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// emptyToolSource satisfies subagent.ToolSource over no tools, standing in for
// the parent registry view production wiring always supplies.
type emptyToolSource struct{}

func (emptyToolSource) All() []agent.Tool    { return nil }
func (emptyToolSource) ReadOnly(string) bool { return false }

// wiredBarrier builds a production-shaped barrier over reg: the inputs both
// front ends install unconditionally, with no-op sinks and an unsure classifier
// standing in for tests that never reach a classification.
func wiredBarrier(reg *tools.Registry) *permit.Barrier {
	return permit.NewBarrier(reg.ReadOnly, permit.Options{
		Classifier: func(context.Context, permit.Subject) permit.Class { return permit.ClassUnsure },
		Notice:     func(string) {},
		DryRun:     reg.DryRun,
	})
}

func TestWireBatchHooks(t *testing.T) {
	t.Parallel()

	barrier := wiredBarrier(tools.New())

	// Driver and RunHeadless must wire before agent.New copies Options: a hook
	// attached to the local afterwards never reaches the running agent.
	t.Run("hook_reaches_a_live_turn", func(t *testing.T) {
		var opts agent.Options
		sag := subagent.New(subagent.Options{Root: t.Context()})
		wireBatchHooks(&opts, sag, barrier)
		require.NotNil(t, opts.OnToolBatch)

		fired := make(chan struct{}, 1)
		hook := func(ctx context.Context, calls []agent.ToolCall) {
			opts.OnToolBatch(ctx, calls) // the wired hook: a real manager and barrier
			select {
			case fired <- struct{}{}:
			default:
			}
		}

		p := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{
			{Events: mainToolTurn()},
			{Events: textTurnRewind("done")},
		}}
		a := agent.New(&agent.State{Model: llm.Model{ID: "test"}, Tokens: tokens.New(llm.Model{ID: "test"})}, agent.Options{
			Provider:    func(llm.Model) (llm.Provider, error) { return p, nil },
			Sinks:       []agent.Sink{agent.NopSink{}},
			Tools:       singleToolSet{tool: noopRewindTool{}},
			Env:         agent.Environment{Cwd: "/repo", OS: "linux/amd64"},
			OnToolBatch: hook,
		})
		require.NoError(t, a.Prompt(t.Context(), agent.Input{Text: "hi"}))
		require.Eventually(t, func() bool {
			select {
			case <-fired:
				return true
			default:
				return false
			}
		}, time.Second, 10*time.Millisecond)
	})
}

// TestClassifierAdapter pins the Classify fallbacks and metadata handling:
// shell commands classify with fresh context, failures and truncated verdicts
// read as unsure, MCP calls need their own metadata to be judged.
func TestClassifierAdapter(t *testing.T) {
	t.Run("classifies_shell_commands", func(t *testing.T) {
		model := llm.Model{ID: "p/m", Provider: "scripted"}
		adapterFor := func(reply string) classifierAdapter {
			sp := newScripted(reply)
			return classifierAdapter{
				providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
				model:       func() llm.Model { return model },
			}
		}

		t.Run("allow_verdict", func(t *testing.T) {
			assert.Equal(t, permit.ClassAllow, adapterFor("allow").Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"}))
		})

		t.Run("deny_verdict_with_noise", func(t *testing.T) {
			assert.Equal(t, permit.ClassDeny, adapterFor("DENY: it modifies the file!").Classify(t.Context(), permit.Subject{Name: "bash", Args: "rm a"}))
		})

		t.Run("garbled_maps_to_unsure", func(t *testing.T) {
			assert.Equal(t, permit.ClassUnsure, adapterFor("? maybe 42").Classify(t.Context(), permit.Subject{Name: "bash", Args: "weird cmd"}))
		})
	})

	t.Run("failures_are_unsure", func(t *testing.T) {
		cases := []struct {
			name string
			ad   classifierAdapter
		}{
			{"no_model_configured", classifierAdapter{ // zero model means no provider
				providerFor: func(llm.Model) (llm.Provider, error) { return nil, nil },
				model:       func() llm.Model { return llm.Model{} },
			}},
			{"provider_error", classifierAdapter{
				providerFor: func(llm.Model) (llm.Provider, error) { return nil, errors.New("no provider") },
				model:       func() llm.Model { return llm.Model{ID: "p/m"} },
			}},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, permit.ClassUnsure, tc.ad.Classify(t.Context(), permit.Subject{Name: "bash", Args: "anything"}))
			})
		}
	})

	t.Run("fresh_context_no_reasoning", func(t *testing.T) {
		model := llm.Model{ID: "p/m", Provider: "scripted", MaxOutput: 64000}
		sp := newScripted("readonly")
		adapter := classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
			model:       func() llm.Model { return model },
		}

		_ = adapter.Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"})
		reqs := sp.Requests()
		require.Len(t, reqs, 1)
		r := reqs[0]
		assert.Equal(t, llm.RoleUser, r.Messages[0].Role) // the command is the whole user turn
		var sys strings.Builder
		for _, b := range r.System {
			if tb, ok := b.(llm.TextBlock); ok {
				sys.WriteString(tb.Text)
			}
		}
		assert.Contains(t, sys.String(), `"allow"`)
		// the classifier never reasons, off stays off whatever the model supports
		assert.Equal(t, llm.ClampLevel(model, llm.LevelOff), r.Reasoning.Level)
	})

	t.Run("classifies_mcp_with_metadata", func(t *testing.T) {
		model := llm.Model{ID: "p/m", Provider: "scripted"}
		sp := newScripted("allow")
		adapter := classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
			model:       func() llm.Model { return model },
			schema: func(name string) (llm.ToolSchema, bool) {
				if name != "srv__list" {
					return llm.ToolSchema{}, false
				}
				return llm.ToolSchema{Name: name, Description: "lists things", Parameters: []byte(`{"type":"object"}`)}, true
			},
		}

		assert.Equal(t, permit.ClassAllow, adapter.Classify(t.Context(), permit.Subject{Name: "srv__list", Args: `{}`}))

		reqs := sp.Requests()
		require.Len(t, reqs, 1)
		var sys strings.Builder
		for _, b := range reqs[0].System {
			if tb, ok := b.(llm.TextBlock); ok {
				sys.WriteString(tb.Text)
			}
		}
		// the MCP prompt embeds description and parameters so the model can judge it
		assert.Contains(t, sys.String(), "lists things")
		assert.Contains(t, sys.String(), `{"type":"object"}`)
	})

	t.Run("unknown_mcp_safely_unsure", func(t *testing.T) {
		adapter := classifierAdapter{ // no MCP metadata: an MCP call cannot be judged
			providerFor: func(llm.Model) (llm.Provider, error) { return nil, nil },
			model:       func() llm.Model { return llm.Model{ID: "p/m"} },
			schema:      func(string) (llm.ToolSchema, bool) { return llm.ToolSchema{}, false },
		}
		assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "srv__list", Args: `{}`}))

		adapter.schema = func(string) (llm.ToolSchema, bool) { return llm.ToolSchema{}, false } // unknown tool
		assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "ghost", Args: `{}`}))
	})

	t.Run("truncated_verdict_is_unsure", func(t *testing.T) {
		sp := &llm.ScriptedProvider{Turns: []llm.ScriptedTurn{{Events: truncatedStream("readonly")}}}
		adapter := classifierAdapter{
			providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
			model:       func() llm.Model { return llm.Model{ID: "p/m", Provider: "scripted"} },
		}

		assert.Equal(t, permit.ClassUnsure, adapter.Classify(t.Context(), permit.Subject{Name: "bash", Args: "stat a"}))
	})

	t.Run("selects_prompt_per_rule_set", func(t *testing.T) {
		model := llm.Model{ID: "p/m", Provider: "scripted"}
		adapterFor := func(reply string) (classifierAdapter, *llm.ScriptedProvider) {
			sp := newScripted(reply)
			return classifierAdapter{
				providerFor: func(llm.Model) (llm.Provider, error) { return sp, nil },
				model:       func() llm.Model { return model },
				schema: func(string) (llm.ToolSchema, bool) {
					return llm.ToolSchema{Name: "mcp__x", Description: "d", Parameters: []byte(`{}`)}, true
				},
				cwd: "/work/proj",
				tmp: "/tmp",
			}, sp
		}
		systemOf := func(sp *llm.ScriptedProvider) string {
			reqs := sp.Requests()
			require.Len(t, reqs, 1)
			require.Len(t, reqs[0].System, 1)
			tb, ok := reqs[0].System[0].(llm.TextBlock)
			require.True(t, ok)
			return tb.Text
		}

		t.Run("write_shell_uses_workspace_prompt", func(t *testing.T) {
			a, sp := adapterFor("allow")
			got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "python3 -c x", AllowWrite: true})
			assert.Equal(t, permit.ClassAllow, got)
			assert.Equal(t, permit.WorkspaceClassifierSystem("/work/proj", "/tmp"), systemOf(sp))
		})

		t.Run("allow_write_shell_denies", func(t *testing.T) {
			a, _ := adapterFor("deny")
			got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "curl x", AllowWrite: true})
			assert.Equal(t, permit.ClassDeny, got)
		})

		t.Run("allow_write_rejects_readonly_word", func(t *testing.T) {
			a, _ := adapterFor("readonly")
			got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "ls", AllowWrite: true})
			assert.Equal(t, permit.ClassUnsure, got) // the retired vocabulary no longer approves
		})

		t.Run("plain_shell_keeps_strict_prompt", func(t *testing.T) {
			a, sp := adapterFor("allow")
			got := a.Classify(t.Context(), permit.Subject{Name: "bash", Args: "ls"})
			assert.Equal(t, permit.ClassAllow, got)
			assert.Equal(t, permit.ClassifierSystem, systemOf(sp))
		})

		t.Run("mcp_keeps_strict_prompt", func(t *testing.T) {
			a, sp := adapterFor("allow")
			got := a.Classify(t.Context(), permit.Subject{Name: "mcp__x", Args: `{}`, AllowWrite: true})
			assert.Equal(t, permit.ClassAllow, got)
			assert.Contains(t, systemOf(sp), "You decide whether a single tool invocation may run unattended")
		})
	})
}

func TestPromptAdapterPlainModeReportsNoUI(t *testing.T) {
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)

	ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
	require.NoError(t, err)
	t.Cleanup(func() {
		ui.Close()
		_ = inR.Close()
		_ = inW.Close()
		_ = outR.Close()
		_ = outW.Close()
	})

	a := promptAdapter{ui: wrapUI(t, ui), hold: func(context.Context) {}}
	_, err = a.Open("Allow?", "cmd", []string{"Allow"})
	assert.ErrorIs(t, err, tui.ErrNoUI)
}
