package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlags(t *testing.T) {
	t.Parallel()

	t.Run("version_flag", func(t *testing.T) {
		f, err := parseFlags([]string{"--version"})
		require.NoError(t, err)
		assert.True(t, f.version)

		f, err = parseFlags([]string{"-v"})
		require.NoError(t, err)
		assert.True(t, f.version)
	})

	t.Run("update_flag", func(t *testing.T) {
		f, err := parseFlags([]string{"--update"})
		require.NoError(t, err)
		assert.True(t, f.update)
	})

	t.Run("long_and_short_prompt", func(t *testing.T) {
		f, err := parseFlags([]string{"-p", "hello"})
		require.NoError(t, err)
		assert.Equal(t, "hello", f.prompt)

		f, err = parseFlags([]string{"--prompt=hello there"})
		require.NoError(t, err)
		assert.Equal(t, "hello there", f.prompt)
	})

	t.Run("output_and_scopes", func(t *testing.T) {
		f, err := parseFlags([]string{"-p", "x", "-o", "json", "--read-only"})
		require.NoError(t, err)
		assert.Equal(t, "json", f.output)
		assert.True(t, f.readOnly)
		assert.Equal(t, scopeReadOnly, f.scope())
	})

	t.Run("tool_lists_split", func(t *testing.T) {
		f, err := parseFlags([]string{"-p", "x", "--allow-tools", "bash,jq", "--deny-tools=write"})
		require.NoError(t, err)
		assert.Equal(t, []string{"bash", "jq"}, f.allowTools)
		assert.Equal(t, []string{"write"}, f.denyTools)
	})

	t.Run("resume_id_lifted_out", func(t *testing.T) {
		f, err := parseFlags([]string{"--resume", "abc123", "-m", "p/m"})
		require.NoError(t, err)
		assert.True(t, f.resume)
		assert.Equal(t, "abc123", f.resumeID)
		assert.Equal(t, "p/m", f.model)
	})

	t.Run("positional_args_kept", func(t *testing.T) {
		f, err := parseFlags([]string{"--render", "plain", "write", "a", "test"})
		require.NoError(t, err)
		assert.Equal(t, "plain", f.render)
		assert.Equal(t, []string{"write", "a", "test"}, f.args)
	})

	t.Run("headless_flags_recorded", func(t *testing.T) {
		f, err := parseFlags([]string{"--allow-all"})
		require.NoError(t, err)
		assert.Equal(t, []string{"--allow-all"}, f.headless)

		f, err = parseFlags(nil)
		require.NoError(t, err)
		assert.Empty(t, f.headless)
	})

	t.Run("unknown_flag_errors", func(t *testing.T) {
		_, err := parseFlags([]string{"--nope"})
		assert.Error(t, err)
	})

	t.Run("help_reports_errhelp", func(t *testing.T) {
		_, err := parseFlags([]string{"-h"})
		assert.ErrorIs(t, err, pflag.ErrHelp)
	})
}

func TestWriteUsage(t *testing.T) {
	t.Parallel()

	fs := pflag.NewFlagSet("ajent", pflag.ContinueOnError)
	var buf bytes.Buffer
	writeUsage(&buf, fs.FlagUsages())
	out := buf.String()
	assert.Equal(t, "ajent version "+config.Version, strings.SplitN(out, "\n", 2)[0])
	assert.Contains(t, out, "usage of ")
	assert.Contains(t, out, "--resume")
}

func TestCliFlagsValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		argv []string
		ok   bool
	}{
		{"bare_interactive", nil, true},
		{"interactive_with_model", []string{"-m", "p/m", "--render", "plain"}, true},
		{"simple_prompt", []string{"-p", "hi"}, true},
		{"prompt_with_allow_all", []string{"-p", "hi", "--allow-all"}, true},
		{"prompt_with_json", []string{"-p", "hi", "-o", "json"}, true},
		{"prompt_with_resume_id", []string{"-p", "hi", "--resume", "abc"}, true},
		{"prompt_with_continue", []string{"-p", "hi", "--continue"}, true},
		{"both_scopes", []string{"-p", "hi", "--allow-all", "--read-only"}, false},
		{"bare_resume_picker", []string{"-p", "hi", "--resume"}, false},
		{"trailing_args", []string{"-p", "hi", "extra"}, false},
		{"unknown_output", []string{"-p", "hi", "-o", "yaml"}, false},
		{"output_without_prompt", []string{"-o", "json"}, false},
		{"read_only_without_prompt", []string{"--read-only"}, false},
		{"deny_tools_without_prompt", []string{"--deny-tools", "bash"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parseFlags(tc.argv)
			require.NoError(t, err)
			if tc.ok {
				assert.NoError(t, f.validate())
			} else {
				assert.Error(t, f.validate())
			}
		})
	}
}

func TestCliFlagsScope(t *testing.T) {
	t.Parallel()

	assert.Equal(t, scopeDefault, cliFlags{}.scope())
	assert.Equal(t, scopeAllowAll, cliFlags{allowAll: true}.scope())
	assert.Equal(t, scopeReadOnly, cliFlags{readOnly: true}.scope())
}
