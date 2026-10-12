package command

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		in        string
		kind      Kind
		rest      string
		excluded  bool
		multiline bool
	}{
		{"plain_prompt", "hello world", KindPrompt, "hello world", false, false},
		{"slash_command", "/model", KindCommand, "model", false, false},
		{"slash_with_args", "/model gpt", KindCommand, "model gpt", false, false},
		{"slash_double_escape", "//literal", KindPrompt, "/literal", false, false},
		{"slash_leading_space", " /notacommand", KindPrompt, "/notacommand", false, false},
		{"shell_command", "!go test ./...", KindShell, "go test ./...", false, false},
		{"shell_double_bang_excluded", "!!echo hi", KindShell, "echo hi", true, false},
		{"shell_triple_bang", "!!!echo hi", KindShell, "!echo hi", true, false},
		{"shell_leading_space_literal", " !echo hi", KindPrompt, "!echo hi", false, false},
		{"bare_slash", "/", KindCommand, "", false, false},
		{"empty", "", KindPrompt, "", false, false},
		{"at_ref_only_prompt", "@main.go and @b.go", KindPrompt, "@main.go and @b.go", false, false},
		{"email_not_command", "email@example.com", KindPrompt, "email@example.com", false, false},
		{"multiline_plain_prompt", "line one\nline two", KindPrompt, "line one\nline two", false, false},
		{"multiline_slash_is_prompt", "/etc/hosts is stale\nfix it", KindPrompt,
			"/etc/hosts is stale\nfix it", false, true},
		{"multiline_shell_still_stages", "!ls\npwd", KindShell, "ls\npwd", false, false},
		{"multiline_bang_bang_stages", "!!echo hi\nbye", KindShell, "echo hi\nbye", true, false},
		{"multiline_space_escape", " //x\ny", KindPrompt, "//x\ny", false, false},
		{"multiline_double_slash_escape", "//literal\nmore", KindPrompt, "/literal\nmore", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseLine(c.in)
			assert.Equal(t, c.kind, got.Kind)
			assert.Equal(t, c.rest, got.Rest)
			assert.Equal(t, c.excluded, got.Excluded)
			assert.Equal(t, c.multiline, got.Multiline)
		})
	}
}

func TestSplitCommand(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rest string
		cmd  string
		arg  string
		ok   bool
	}{
		{"name_and_arg", "model gpt-4", "model", "gpt-4", true},
		{"name_only", "model", "model", "", true},
		{"extra_spaces_trimmed", "reasoning   medium", "reasoning", "medium", true},
		{"bare_slash_no_name", "", "", "", false},
		{"case_lowercased_name", "Model gpt", "model", "gpt", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name, arg, ok := SplitCommand(c.rest)
			assert.Equal(t, c.cmd, name)
			assert.Equal(t, c.arg, arg)
			assert.Equal(t, c.ok, ok)
		})
	}
}
