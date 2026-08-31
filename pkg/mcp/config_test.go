package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadConfig exercises the user-to-project merge by server name with whole-entry
// replacement, env interpolation and validation.
func TestLoadConfig(t *testing.T) {
	t.Run("user_overridden_by_whole_entry", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json", `{"servers":{
		  "srv":{"command":"user-cmd","args":["a"]}
		}}`)
		ws := t.TempDir()
		mkFile(t, ws+"/.ajent/mcp.json", `{"servers":{
		  "srv":{"url":"http://project.example"},
		  "other":{"command":"only-project"}
		}}`)

		got, warns, err := LoadConfig(ws)
		require.NoError(t, err)
		assert.Empty(t, warns)

		s, ok := got["srv"]
		require.True(t, ok)
		assert.Empty(t, s.Command) // project's whole entry replaced user's
		assert.Equal(t, "http://project.example", s.URL)
		_, ok = got["other"]
		assert.True(t, ok)
	})

	t.Run("env_vars_interpolated_in_headers", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		t.Setenv("MCP_TOKEN", "sekret")
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
			`{"servers":{"srv":{
			  "command":"x","headers":{"Authorization":"Bearer ${env:MCP_TOKEN}"}
			}}}`)

		got, _, err := LoadConfig(t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, "Bearer sekret", got["srv"].Headers["Authorization"])
	})

	t.Run("missing_env_var_names_error", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
			`{"servers":{"srv":{"command":"x","headers":{"Authorization":"Bearer ${env:NOPE}"}}}`)

		_, _, err := LoadConfig(t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "NOPE")
	})

	t.Run("command_and_url_ambiguous_rejected", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
			`{"servers":{"srv":{"command":"x","url":"http://y"}}}`)

		_, _, err := LoadConfig(t.TempDir())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "both")
	})

	t.Run("unknown_key_warns", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
			`{"servers":{"srv":{"command":"x","timeuot":"5s"}}}`)

		_, warns, err := LoadConfig(t.TempDir())
		require.NoError(t, err)
		assert.NotEmpty(t, warns) // the typo'd "timeuot" is flagged
	})

	t.Run("timeout_parses_as_milliseconds", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json", `{"servers":{
		  "sectool":{"transport":"http","url":"http://127.0.0.1:9119/mcp","timeout":600000}
		}}`)

		got, warns, err := LoadConfig(t.TempDir())
		require.NoError(t, err)
		assert.Empty(t, warns)
		s := got["sectool"]
		assert.Equal(t, TransportHTTP, s.Transport)
		assert.Equal(t, 10*time.Minute, time.Duration(s.Timeout)) // numeric ms: 600000
	})

	t.Run("stdio_inference_and_readonly_globs", func(t *testing.T) {
		t.Setenv("AJENT_HOME", mkHome(t))
		mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json", `{"servers":{
		  "browser":{"command":"mcp-fetch-go","readOnly":true},
		  "gh":{"transport":"stdio","command":"github-mcp-server","args":["stdio"]}
		}}`)

		got, _, err := LoadConfig(t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, "stdio", transportKind(got["browser"]))           // inferred from command
		assert.Equal(t, "stdio", got["gh"].Transport)                     // explicit
		assert.Equal(t, []string{"*"}, []string(got["browser"].ReadOnly)) // boolean true marks all
	})
}

// TestValidateServer checks each declaration's transport/command/url contract.
func TestValidateServer(t *testing.T) {
	cases := []struct {
		name        string
		cfg         ServerConfig
		errContains string
	}{
		{"stdio_needs_a_command", ServerConfig{Transport: "stdio", URL: "http://x"}, "requires a command"},
		{"network_needs_a_url", ServerConfig{Transport: "http", Command: "x"}, "requires a url"},
		{"command_and_url_both_rejected", ServerConfig{Command: "x", URL: "http://y"}, "not both"},
		{"neither_command_nor_url", ServerConfig{}, "need a command"},
		{"unknown_transport_value", ServerConfig{Transport: "websocket", URL: "http://x"}, `unknown transport "websocket"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateServer("s", tc.cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errContains)
		})
	}
}

// TestExpandVar covers the substitution branches LoadConfig does not reach
// directly: multiple references in one value and an unterminated reference.
func TestExpandVar(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		set         map[string]string
		want        string
		errContains string
	}{
		{"no_reference_unchanged", "plain text", nil, "plain text", ""},
		{"single_substitution", "${env:MCP_TOKEN}", map[string]string{"MCP_TOKEN": "sekret"}, "sekret", ""},
		{"multiple_refs_in_one_value", "a-${env:ONE}-b-${env:TWO}",
			map[string]string{"ONE": "1", "TWO": "2"}, "a-1-b-2", ""},
		{"unterminated_reference_errors", "${env:NOPE", nil, "", "unterminated"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.set {
				t.Setenv(k, v)
			}
			got, err := expandVar(tc.in)
			if tc.errContains != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// mkHome creates a fresh AJENT home dir.
func mkHome(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// mkFile writes content to path creating parent dirs.
func mkFile(t *testing.T, path, content string) {
	t.Helper()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
