package command

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseLine(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		kind Kind
		rest string
	}{
		{"plain_prompt", "hello world", KindPrompt, "hello world"},
		{"slash_command", "/model", KindCommand, "model"},
		{"slash_with_args", "/model gpt", KindCommand, "model gpt"},
		{"slash_double_escape", "//literal", KindPrompt, "/literal"},
		{"slash_leading_space", " /notacommand", KindPrompt, "/notacommand"},
		{"shell_command", "!go test ./...", KindShell, "go test ./..."},
		{"shell_leading_space_literal", " !echo hi", KindPrompt, "!echo hi"},
		{"shell_double_escape", "!!echo hi", KindPrompt, "!echo hi"},
		{"bare_slash", "/", KindCommand, ""},
		{"empty", "", KindPrompt, ""},
		{"at_ref_only_prompt", "@main.go and @b.go", KindPrompt, "@main.go and @b.go"},
		{"email_not_command", "email@example.com", KindPrompt, "email@example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseLine(c.in)
			assert.Equal(t, c.kind, got.Kind)
			assert.Equal(t, c.rest, got.Rest)
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
