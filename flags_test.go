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

	t.Run("session_name_parsed", func(t *testing.T) {
		f, err := parseFlags([]string{"--session", "  fix-parser  ", "-m", "p/m"})
		require.NoError(t, err)
		assert.True(t, f.sessionGiven)
		assert.Equal(t, "fix-parser", f.sessionName)
		assert.Equal(t, "p/m", f.model)
	})

	t.Run("delete_target_parsed", func(t *testing.T) {
		f, err := parseFlags([]string{"--delete", "  fix-parser  "})
		require.NoError(t, err)
		assert.True(t, f.deleteGiven)
		assert.Equal(t, "fix-parser", f.deleteTarget)
	})

	t.Run("bare_delete_old_defaults", func(t *testing.T) {
		f, err := parseFlags([]string{"--delete-old"})
		require.NoError(t, err)
		assert.True(t, f.deleteOld)
		assert.Equal(t, defaultStaleDays, f.deleteOldDays)
	})

	t.Run("delete_old_days_lifted_out", func(t *testing.T) {
		f, err := parseFlags([]string{"--delete-old", "7", "-m", "p/m"})
		require.NoError(t, err)
		assert.True(t, f.deleteOld)
		assert.Equal(t, 7, f.deleteOldDays)
		assert.Equal(t, "p/m", f.model)
	})

	// only a day count is the optional value, so anything else stays a positional
	t.Run("delete_old_keeps_non_digits", func(t *testing.T) {
		f, err := parseFlags([]string{"--delete-old", "soon"})
		require.NoError(t, err)
		assert.True(t, f.deleteOld)
		assert.Equal(t, defaultStaleDays, f.deleteOldDays)
		assert.Equal(t, []string{"soon"}, f.args)
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
	assert.Contains(t, out, "--delete-old")
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
		{"session_name", []string{"--session", "fix-parser"}, true},
		{"prompt_with_session_name", []string{"-p", "hi", "--session", "fix-parser"}, true},
		{"session_with_resume", []string{"--session", "a", "--resume", "b"}, false},
		{"session_with_continue", []string{"--session", "a", "--continue"}, false},
		{"empty_session_name", []string{"--session", ""}, false},
		{"session_name_with_space", []string{"--session", "fix parser"}, false},
		{"delete_target", []string{"--delete", "fix-parser"}, true},
		{"delete_old_bare", []string{"--delete-old"}, true},
		{"delete_old_with_days", []string{"--delete-old", "7"}, true},
		{"delete_with_model", []string{"--delete", "a", "-m", "p/m"}, true},
		{"empty_delete_target", []string{"--delete", "  "}, false},
		{"delete_with_delete_old", []string{"--delete", "a", "--delete-old"}, false},
		{"delete_with_session", []string{"--delete", "a", "--session", "b"}, false},
		{"delete_with_resume", []string{"--delete", "a", "--resume"}, false},
		{"delete_with_prompt", []string{"--delete", "a", "-p", "hi"}, false},
		{"delete_old_with_continue", []string{"--delete-old", "--continue"}, false},
		{"delete_old_zero_days", []string{"--delete-old", "0"}, false},
		{"delete_old_bad_days", []string{"--delete-old", "soon"}, false},
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

	// a bad day count is left as a positional, so it must be reported as the bad
	// value it is rather than as a flag combination
	t.Run("bad_days_names_the_value", func(t *testing.T) {
		f, err := parseFlags([]string{"--delete-old", "soon"})
		require.NoError(t, err)
		require.EqualError(t, f.validate(), `--delete-old takes a number of days, not "soon"`)

		// the = form reports the same thing from the parser
		_, err = parseFlags([]string{"--delete-old=soon"})
		require.EqualError(t, err, `--delete-old takes a number of days, not "soon"`)
	})
}

func TestCliFlagsScope(t *testing.T) {
	t.Parallel()

	assert.Equal(t, scopeDefault, cliFlags{}.scope())
	assert.Equal(t, scopeAllowAll, cliFlags{allowAll: true}.scope())
	assert.Equal(t, scopeReadOnly, cliFlags{readOnly: true}.scope())
}

func TestStatsFlagIsHeadlessOnly(t *testing.T) {
	t.Parallel()

	f, err := parseFlags([]string{"--stats"})
	require.NoError(t, err)

	err = f.validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--stats only applies with --prompt")

	f, err = parseFlags([]string{"-p", "hi", "--stats"})
	require.NoError(t, err)
	assert.NoError(t, f.validate())
	assert.True(t, f.stats)
}
