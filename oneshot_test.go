package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
)

// scopeRegistry mirrors a live headless registry: the built-ins, the sub-agent
// trio, and one MCP server offering a read-only tool, a writer and a tool its
// config left disabled.
func scopeRegistry(t *testing.T) *tools.Registry {
	t.Helper()
	reg, err := tools.Builtins(tools.Options{Cwd: t.TempDir()})
	require.NoError(t, err)

	for _, name := range []string{"agent_start", "agent_poll", "agent_list"} {
		reg.RegisterFrom(tools.SourceBuiltin, &stubTool{name: name}, true)
	}
	reg.MarkReadOnly([]string{"agent_start", "agent_poll", "agent_list"})

	reg.RegisterState("srv", &stubTool{name: "srv__search"}, tools.StateEnabled)
	reg.RegisterState("srv", &stubTool{name: "srv__deploy"}, tools.StateEnabled)
	reg.RegisterState("srv", &stubTool{name: "srv__off"}, tools.StateDisabled)
	reg.MarkReadOnly([]string{"srv__search", "srv__off"})
	return reg
}

func TestHeadlessTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scope       toolScope
		allow, deny []string
		want        []string
	}{
		{
			name:  "default_drops_bash",
			scope: scopeDefault,
			want: []string{"agent_list", "agent_poll", "agent_start", "edit", "find",
				"grep", "ls", "read", "srv__deploy", "srv__search", "write"},
		},
		{
			name:  "allow_all_adds_bash",
			scope: scopeAllowAll,
			want: []string{"agent_list", "agent_poll", "agent_start", "bash", "edit",
				"find", "grep", "ls", "read", "srv__deploy", "srv__search", "write"},
		},
		{
			name:  "read_only_keeps_readers",
			scope: scopeReadOnly,
			want: []string{"agent_list", "agent_poll", "agent_start", "find", "grep",
				"ls", "read", "srv__search"},
		},
		{
			name:  "allow_tools_adds_bash",
			scope: scopeDefault,
			allow: []string{"bash"},
			want: []string{"agent_list", "agent_poll", "agent_start", "bash", "edit",
				"find", "grep", "ls", "read", "srv__deploy", "srv__search", "write"},
		},
		{
			name:  "deny_tools_wins",
			scope: scopeAllowAll,
			allow: []string{"write"},
			deny:  []string{"write", "edit", "bash"},
			want: []string{"agent_list", "agent_poll", "agent_start", "find", "grep",
				"ls", "read", "srv__deploy", "srv__search"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := headlessTools(scopeRegistry(t), tc.scope, tc.allow, tc.deny)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("applies_to_the_registry", func(t *testing.T) {
		reg := scopeRegistry(t)
		reg.SetEnabled(headlessTools(reg, scopeReadOnly, nil, nil))
		_, ok := reg.Get("write")
		assert.False(t, ok) // Get only returns enabled tools
		_, ok = reg.Get("grep")
		assert.True(t, ok)
	})
}

func TestHeadlessOutcome(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		err    error
		res    agent.TurnResult
		answer string
		status string
		code   int
	}{
		{"clean_answer", nil, agent.TurnResult{Stop: llm.StopEndTurn}, "hi", statusOK, exitOK},
		{"prompt_error", errors.New("boom"), agent.TurnResult{}, "hi", statusError, exitTurn},
		{"turn_error", nil, agent.TurnResult{Err: errors.New("boom")}, "hi", statusError, exitTurn},
		{"stop_error", nil, agent.TurnResult{Stop: llm.StopError}, "hi", statusError, exitTurn},
		{"aborted", nil, agent.TurnResult{Stop: llm.StopAborted}, "hi", statusEmpty, exitTurn},
		{"no_answer", nil, agent.TurnResult{Stop: llm.StopEndTurn}, "", statusEmpty, exitTurn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, code := headlessOutcome(tc.err, tc.res, tc.answer)
			assert.Equal(t, tc.status, status)
			assert.Equal(t, tc.code, code)
		})
	}
}

func TestFinalAnswer(t *testing.T) {
	t.Parallel()

	t.Run("last_assistant_wins", func(t *testing.T) {
		msgs := []llm.Message{
			llm.Text(llm.RoleAssistant, "first"),
			llm.Text(llm.RoleUser, "again"),
			llm.Text(llm.RoleAssistant, "second"),
		}
		assert.Equal(t, "second", finalAnswer(msgs))
	})

	t.Run("joins_text_blocks", func(t *testing.T) {
		msgs := []llm.Message{{Role: llm.RoleAssistant, Content: llm.BlockList{
			llm.ThinkingBlock{Text: "hidden"},
			llm.TextBlock{Text: "one"},
			llm.TextBlock{Text: " "},
			llm.TextBlock{Text: "two"},
		}}}
		assert.Equal(t, "one\ntwo", finalAnswer(msgs))
	})

	t.Run("no_assistant_message", func(t *testing.T) {
		assert.Empty(t, finalAnswer(nil))
		assert.Empty(t, finalAnswer([]llm.Message{llm.Text(llm.RoleUser, "hi")}))
	})
}

// textAndCallTurn scripts one step that speaks before calling a tool, so a
// multi-step turn has prose to stream ahead of its final answer.
func textAndCallTurn(text, id, name, args string) []llm.Event {
	return []llm.Event{
		{Type: llm.EventMessageStart},
		{Type: llm.EventTextStart, Index: 0},
		{Type: llm.EventTextDelta, Index: 0, Text: text},
		{Type: llm.EventTextEnd, Index: 0, Block: llm.TextBlock{Text: text}},
		{Type: llm.EventToolCallStart, Index: 1, ToolCallID: id, ToolName: name},
		{Type: llm.EventToolCallEnd, Index: 1, Block: llm.ToolCallBlock{
			ID: id, Name: name, Input: json.RawMessage(args)}},
		{Type: llm.EventDone, StopReason: llm.StopToolUse},
	}
}

// headlessHarness isolates a run: its own workspace, AJENT_HOME and scripted
// provider, so nothing reaches the developer's real config or transcripts. A
// non-empty projectCfg is written as the workspace's .ajent/config.json.
func headlessHarness(t *testing.T, f cliFlags, projectCfg string, turns []llm.ScriptedTurn) (int, string, string) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("AJENT_HOME", t.TempDir())
	if projectCfg != "" {
		require.NoError(t, os.MkdirAll(".ajent", 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(".ajent", "config.json"), []byte(projectCfg), 0o600))
	}

	set, _, err := config.Load(config.Options{Workspace: cwdOrDot()})
	require.NoError(t, err)
	reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
	model := llm.Model{Provider: "p", ID: "m", ContextWindow: 100000}
	sp := &llm.ScriptedProvider{ProviderName: "p", Turns: turns}

	var out, errw bytes.Buffer
	code := runHeadless(headlessOptions{
		flags: f, set: set, reg: reg, active: model, sessMode: modeNewSession,
		out: &out, errw: &errw,
		provider: func(llm.Model) (llm.Provider, error) { return sp, nil },
	})
	return code, out.String(), errw.String()
}

func TestRunHeadless(t *testing.T) {
	t.Run("text_answer_exits_zero", func(t *testing.T) {
		code, out, _ := headlessHarness(t, cliFlags{prompt: "hi", output: outputText}, "",
			[]llm.ScriptedTurn{{Events: textTurn("all done")}})
		assert.Equal(t, exitOK, code)
		assert.Equal(t, "all done\n", out)
	})

	t.Run("json_stream_ends_in_result", func(t *testing.T) {
		code, out, _ := headlessHarness(t, cliFlags{prompt: "hi", output: outputJSON}, "",
			[]llm.ScriptedTurn{{Events: textTurn("all done")}})
		assert.Equal(t, exitOK, code)

		lines := decodeLines(t, out)
		require.NotEmpty(t, lines)
		last := lines[len(lines)-1]
		assert.Equal(t, "result", last["type"])
		assert.Equal(t, statusOK, last["status"])
		assert.Equal(t, "all done", last["text"])
	})

	t.Run("empty_answer_exits_two", func(t *testing.T) {
		code, out, _ := headlessHarness(t, cliFlags{prompt: "hi", output: outputText}, "",
			[]llm.ScriptedTurn{{Events: textTurn("")}})
		assert.Equal(t, exitTurn, code)
		assert.Empty(t, out)
	})

	t.Run("provider_error_exits_two", func(t *testing.T) {
		code, _, _ := headlessHarness(t, cliFlags{prompt: "hi", output: outputText}, "",
			[]llm.ScriptedTurn{{Err: errors.New("no credentials")}})
		assert.Equal(t, exitTurn, code)
	})

	t.Run("tool_call_runs_at_allow_all", func(t *testing.T) {
		code, out, _ := headlessHarness(t,
			cliFlags{prompt: "hi", output: outputJSON, allowAll: true}, "",
			[]llm.ScriptedTurn{
				{Events: devCallTurn("c1", "bash", `{"command":"echo hello"}`)},
				{Events: textTurn("it said hello")},
			})
		assert.Equal(t, exitOK, code)

		lines := decodeLines(t, out)
		var result map[string]any
		for _, l := range lines {
			if l["type"] == "tool_result" {
				result = l
			}
		}
		require.NotNil(t, result)
		assert.NotContains(t, result, "error")
		assert.Contains(t, result["output"], "hello")
	})

	t.Run("denied_command_still_completes", func(t *testing.T) {
		code, out, _ := headlessHarness(t,
			cliFlags{prompt: "hi", output: outputJSON, allowAll: true},
			`{"permissions":{"deniedCommands":["echo"]}}`,
			[]llm.ScriptedTurn{
				{Events: devCallTurn("c1", "bash", `{"command":"echo hello"}`)},
				{Events: textTurn("that was refused")},
			})
		assert.Equal(t, exitOK, code) // a denial is a tool result; the turn adapts

		lines := decodeLines(t, out)
		var result map[string]any
		for _, l := range lines {
			if l["type"] == "tool_result" {
				result = l
			}
		}
		require.NotNil(t, result)
		assert.Equal(t, true, result["error"])
		assert.Contains(t, result["output"], "denied by configuration")
	})

	t.Run("text_streams_every_step", func(t *testing.T) {
		code, out, errw := headlessHarness(t,
			cliFlags{prompt: "hi", output: outputText, allowAll: true}, "",
			[]llm.ScriptedTurn{
				{Events: textAndCallTurn("let me check", "c1", "bash", `{"command":"echo hello"}`)},
				{Events: textTurn("all done")},
			})
		assert.Equal(t, exitOK, code)
		assert.Equal(t, "let me check\nall done\n", out) // prose from both steps, in order
		assert.Contains(t, errw, "ajent: tool: ")        // progress never touches stdout
	})

	t.Run("expands_at_references", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		t.Setenv("AJENT_HOME", t.TempDir())
		require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o600))

		set, _, err := config.Load(config.Options{Workspace: cwdOrDot()})
		require.NoError(t, err)
		reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
		sp := &llm.ScriptedProvider{ProviderName: "p",
			Turns: []llm.ScriptedTurn{{Events: textTurn("package a")}}}

		var out, errw bytes.Buffer
		code := runHeadless(headlessOptions{
			flags:  cliFlags{prompt: "explain @a.go", output: outputText},
			set:    set,
			reg:    reg,
			active: llm.Model{Provider: "p", ID: "m", ContextWindow: 100000},
			out:    &out, errw: &errw,
			provider: func(llm.Model) (llm.Provider, error) { return sp, nil },
		})
		require.Equal(t, exitOK, code)

		reqs := sp.Requests()
		require.Len(t, reqs, 1)
		msgs := reqs[0].Messages
		require.Len(t, msgs, 3)
		assert.Equal(t, llm.TextBlock{Text: "explain @a.go"}, msgs[0].Content[0])
		// the read follows the message that asked for it
		call, ok := msgs[1].Content[0].(llm.ToolCallBlock)
		require.True(t, ok)
		assert.Equal(t, "read", call.Name)
		res, ok := msgs[2].Content[0].(llm.ToolResultBlock)
		require.True(t, ok)
		assert.Equal(t, call.ID, res.CallID)
	})

	t.Run("continue_resumes_the_transcript", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("AJENT_HOME", t.TempDir())
		set, _, err := config.Load(config.Options{Workspace: cwdOrDot()})
		require.NoError(t, err)
		reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})
		model := llm.Model{Provider: "p", ID: "m", ContextWindow: 100000}

		run := func(prompt string, mode resumeMode, turns []llm.ScriptedTurn) *llm.ScriptedProvider {
			sp := &llm.ScriptedProvider{ProviderName: "p", Turns: turns}
			var out, errw bytes.Buffer
			code := runHeadless(headlessOptions{
				flags: cliFlags{prompt: prompt, output: outputText}, set: set, reg: reg,
				active: model, sessMode: mode, out: &out, errw: &errw,
				provider: func(llm.Model) (llm.Provider, error) { return sp, nil },
			})
			require.Equal(t, exitOK, code, errw.String())
			return sp
		}

		run("first question", modeNewSession, []llm.ScriptedTurn{{Events: textTurn("first answer")}})
		sp := run("second question", modeContinue, []llm.ScriptedTurn{{Events: textTurn("second answer")}})

		// the resumed branch reaches the provider, so the model sees the prior turn
		reqs := sp.Requests()
		require.NotEmpty(t, reqs)
		var seen []string
		for _, m := range reqs[0].Messages {
			for _, b := range m.Content {
				if tb, ok := b.(llm.TextBlock); ok {
					seen = append(seen, tb.Text)
				}
			}
		}
		assert.Contains(t, seen, "first question")
		assert.Contains(t, seen, "first answer")
		assert.Contains(t, seen, "second question")
	})

	t.Run("no_model_exits_one", func(t *testing.T) {
		t.Chdir(t.TempDir())
		t.Setenv("AJENT_HOME", t.TempDir())
		set, _, err := config.Load(config.Options{Workspace: cwdOrDot()})
		require.NoError(t, err)
		reg, _ := llm.NewRegistry(llm.File{}, nil, llm.RegistryOptions{})

		var out, errw bytes.Buffer
		code := runHeadless(headlessOptions{
			flags: cliFlags{prompt: "hi"}, set: set, reg: reg,
			sessMode: modeNewSession, out: &out, errw: &errw,
		})
		assert.Equal(t, exitUsage, code)
		assert.Contains(t, errw.String(), "no model configured")
	})
}
