package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHelpDocs(t *testing.T) {
	t.Parallel()

	docs := helpDocs()
	require.NotEmpty(t, docs)

	bodies := make(map[string]string, len(docs))
	for _, d := range docs {
		bodies[d.Name] = d.Body
	}

	t.Run("readme_first_all_non_empty", func(t *testing.T) {
		assert.Equal(t, "README.md", docs[0].Name)
		for name, body := range bodies {
			assert.NotEmpty(t, strings.TrimSpace(body), name)
		}
	})

	// one anchor topic per features file, so a file cannot quietly go stub
	t.Run("per_file_anchors", func(t *testing.T) {
		anchors := map[string]string{
			"docs/features/permissions.md": "safeCommands",
			"docs/features/agents.md":      "/plan",
			"docs/features/mcp.md":         "/mcp",
			"docs/features/sessions.md":    "control socket",
			"docs/features/cli.md":         "--no-tools",
			"docs/features/tui.md":         "Shift+Tab",
			"docs/features/commands.md":    "!!",
			"docs/features/config.md":      "AJENT_",
		}
		for name, anchor := range anchors {
			body, ok := bodies[name]
			require.True(t, ok, name)
			assert.Contains(t, body, anchor)
		}
	})

	t.Run("readme_headings_covered", func(t *testing.T) {
		var features strings.Builder
		for name, body := range bodies {
			if name != "README.md" {
				features.WriteString(body)
			}
		}

		for line := range strings.Lines(bodies["README.md"]) {
			heading, ok := strings.CutPrefix(line, "## ")
			if !ok {
				continue
			}
			assert.Contains(t, strings.ToLower(features.String()), strings.ToLower(strings.TrimSpace(heading)))
		}
	})
}
