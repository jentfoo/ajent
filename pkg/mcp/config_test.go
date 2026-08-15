package mcp

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadConfigProjectOverridesUser verifies whole-entry replacement by name.
func TestLoadConfigProjectOverridesUser(t *testing.T) {
	t.Setenv("AJENT_HOME", mkHome(t))
	home := os.Getenv("AJENT_HOME")
	mkFile(t, home+"/mcp.json", `{"servers":{
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
}

func TestLoadConfigEnvInterpolation(t *testing.T) {
	t.Setenv("AJENT_HOME", mkHome(t))
	t.Setenv("MCP_TOKEN", "sekret")
	mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
		`{"servers":{"srv":{
		  "command":"x","headers":{"Authorization":"Bearer ${env:MCP_TOKEN}"}
		}}}`)

	got, _, err := LoadConfig(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, "Bearer sekret", got["srv"].Headers["Authorization"])
}

func TestLoadConfigMissingEnvIsError(t *testing.T) {
	t.Setenv("AJENT_HOME", mkHome(t))
	mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
		`{"servers":{"srv":{"command":"x","headers":{"Authorization":"Bearer ${env:NOPE}"}}}`)

	_, _, err := LoadConfig(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NOPE")
}

func TestLoadConfigAmbiguousTransportIsError(t *testing.T) {
	t.Setenv("AJENT_HOME", mkHome(t))
	mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
		`{"servers":{"srv":{"command":"x","url":"http://y"}}}`)

	_, _, err := LoadConfig(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both")
}

func TestLoadConfigUnknownKeyWarns(t *testing.T) {
	t.Setenv("AJENT_HOME", mkHome(t))
	mkFile(t, os.Getenv("AJENT_HOME")+"/mcp.json",
		`{"servers":{"srv":{"command":"x","timeuot":"5s"}}}`)

	_, warns, err := LoadConfig(t.TempDir())
	require.NoError(t, err)
	assert.NotEmpty(t, warns) // the typo'd "timeuot" is flagged
}

func TestLoadConfigPiFormatNumericTimeout(t *testing.T) {
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
}

func TestLoadConfigStdioServer(t *testing.T) {
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
}

func TestValidateServerStdioNeedsCommand(t *testing.T) {
	err := validateServer("s", ServerConfig{Transport: "stdio", URL: "http://x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a command")
}

func TestValidateServerNetworkNeedsURL(t *testing.T) {
	err := validateServer("s", ServerConfig{Transport: "http", Command: "x"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires a url")
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
