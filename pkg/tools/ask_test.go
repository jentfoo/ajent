package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// askCall runs the tool with the given params and returns its result text.
func askCall(t *testing.T, tool *askUserTool, params askParams) agent.ToolResult {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	res, err := tool.Execute(t.Context(), agent.ToolCall{Name: "ask_user", Input: raw}, nil)
	require.NoError(t, err) // a question never fails a turn
	return res
}

func TestAskUserToolExecute(t *testing.T) {
	t.Parallel()

	t.Run("chosen_option", func(t *testing.T) {
		tool := &askUserTool{ask: func(context.Context, string, []string) (int, string, bool, error) {
			return 1, "", false, nil
		}}
		res := askCall(t, tool, askParams{Question: "which?", Options: []string{"a", "b"}})
		assert.False(t, res.IsError)
		assert.Contains(t, res.Display, "The user chose: b")
	})

	t.Run("free_text_answer", func(t *testing.T) {
		tool := &askUserTool{ask: func(context.Context, string, []string) (int, string, bool, error) {
			return 0, "use postgres", false, nil
		}}
		res := askCall(t, tool, askParams{Question: "which store?"})
		assert.Contains(t, res.Display, "use postgres")
	})

	t.Run("reply_instead_of_option", func(t *testing.T) {
		tool := &askUserTool{ask: func(context.Context, string, []string) (int, string, bool, error) {
			return -1, "neither, split it in two", false, nil
		}}
		res := askCall(t, tool, askParams{Question: "which?", Options: []string{"a", "b"}})
		assert.False(t, res.IsError)
		assert.Contains(t, res.Display, "chose none of the options")
		assert.Contains(t, res.Display, "neither, split it in two")
	})

	t.Run("declined_is_not_error", func(t *testing.T) {
		tool := &askUserTool{ask: func(context.Context, string, []string) (int, string, bool, error) {
			return 0, "", true, nil
		}}
		res := askCall(t, tool, askParams{Question: "which?", Options: []string{"a", "b"}})
		assert.False(t, res.IsError)
		assert.Contains(t, res.Display, "declined")
	})

	t.Run("no_ui_reports_back", func(t *testing.T) {
		res := askCall(t, &askUserTool{}, askParams{Question: "which?"})
		assert.False(t, res.IsError)
		assert.Contains(t, res.Display, "no interactive terminal")
	})

	t.Run("asker_error_reports_back", func(t *testing.T) {
		tool := &askUserTool{ask: func(context.Context, string, []string) (int, string, bool, error) {
			return 0, "", false, errors.New("closed")
		}}
		res := askCall(t, tool, askParams{Question: "which?"})
		assert.False(t, res.IsError)
		assert.Contains(t, res.Display, "closed")
	})

	t.Run("empty_question_errors", func(t *testing.T) {
		res := askCall(t, &askUserTool{}, askParams{Question: "  "})
		assert.True(t, res.IsError)
	})
}

func TestBuiltinsRegistersAskUserDisabled(t *testing.T) {
	t.Parallel()

	reg, err := Builtins(Options{Cwd: t.TempDir()})
	require.NoError(t, err)
	assert.NotContains(t, reg.Names(), "ask_user") // disabled until a workflow enables it
	assert.Contains(t, reg.AllNames(SourceBuiltin), "ask_user")
	assert.True(t, reg.ReadOnly("ask_user"))
}
