package app

import (
	"errors"
	"slices"
	"testing"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// missingOnPath fails lookPath for names, accepts the rest.
func missingOnPath(missing ...string) func(string) (string, error) {
	return func(name string) (string, error) {
		for _, m := range missing {
			if name == m {
				return "", errors.New(name + ": not found")
			}
		}
		return "/usr/bin/" + name, nil
	}
}

func TestAdvertisedCommands(t *testing.T) {
	// no t.Parallel: cases derive expectations from the shared probe set
	t.Run("keeps_installed", func(t *testing.T) {
		got, missing := advertisedCommands(config.Settings{}, missingOnPath())
		assert.Equal(t, tools.ShellExamples, got)
		assert.Empty(t, missing)
	})

	t.Run("drops_missing_binaries", func(t *testing.T) {
		set := config.Settings{}
		set.Tools.ShellCommands = []string{"go", "rg"}
		got, missing := advertisedCommands(set, missingOnPath("wc", "rg"))
		assert.Equal(t, []string{"ls", "grep", "find", "diff", "go"}, got)
		assert.Equal(t, []string{"wc", "rg"}, missing)
	})

	t.Run("drops_bare_denied", func(t *testing.T) {
		set := config.Settings{}
		set.Permissions.DeniedCommands = []string{"ls"}
		got, _ := advertisedCommands(set, missingOnPath())
		assert.Equal(t, []string{"grep", "find", "diff", "wc"}, got)
	})

	t.Run("drops_parent_of_denied_subcommand", func(t *testing.T) {
		set := config.Settings{}
		set.Permissions.DeniedCommands = []string{"git stash"}
		set.Tools.ShellCommands = []string{"git", "make"}
		got, _ := advertisedCommands(set, missingOnPath())
		assert.Equal(t, slices.Concat(tools.ShellExamples, []string{"make"}), got)
	})

	t.Run("drops_extra_matching_denied_prefix", func(t *testing.T) {
		set := config.Settings{}
		set.Permissions.DeniedCommands = []string{"git"}
		set.Tools.ShellCommands = []string{"git status"}
		got, _ := advertisedCommands(set, missingOnPath())
		assert.Equal(t, tools.ShellExamples, got)
	})

	t.Run("bash_denied_advertises_nothing", func(t *testing.T) {
		set := config.Settings{}
		set.Permissions.DeniedCommands = []string{"bash"}
		got, missing := advertisedCommands(set, missingOnPath())
		assert.Nil(t, got)
		assert.Nil(t, missing)
	})

	t.Run("drops_enabled_tool_names", func(t *testing.T) {
		set := config.Settings{}
		set.Tools.Enabled = []string{"read", "grep"}
		got, _ := advertisedCommands(set, missingOnPath())
		assert.Equal(t, []string{"ls", "find", "diff", "wc"}, got)
	})

	t.Run("trims_whitespace", func(t *testing.T) {
		set := config.Settings{}
		set.Tools.ShellCommands = []string{" go ", "  "}
		got, _ := advertisedCommands(set, missingOnPath())
		assert.Equal(t, slices.Concat(tools.ShellExamples, []string{"go"}), got)
	})

	t.Run("dedupes_config_extras", func(t *testing.T) {
		set := config.Settings{}
		set.Tools.ShellCommands = []string{"grep", "go", "go"}
		got, _ := advertisedCommands(set, missingOnPath())
		assert.Equal(t, slices.Concat(tools.ShellExamples, []string{"go"}), got)
	})
}

func TestBuiltinTools(t *testing.T) {
	t.Parallel()

	set := loadTestConfig(t)
	require.NoError(t, set.SetSession("tools.shellCommands", []string{"diff", "ajent-no-such-bin"}))

	var warns []string
	reg, err := builtinTools(set, nil, func(msg string) { warns = append(warns, msg) })
	require.NoError(t, err)
	require.NotNil(t, reg)

	var desc string
	for _, s := range reg.Schemas() {
		if s.Name == tools.ToolBash {
			desc = s.Description
		}
	}
	// the bogus name warns; diff is real everywhere and lands in the description
	assert.Contains(t, desc, "Example available commands:")
	assert.Contains(t, desc, "diff")
	assert.Len(t, warns, 1)
	assert.Contains(t, warns[0], "ajent-no-such-bin")
}
