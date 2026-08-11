package history

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoadSaveRoundTrip verifies history round-trips through disk, deduped.
// Not parallel: t.Setenv is incompatible with t.Parallel.
func TestLoadSaveRoundTrip(t *testing.T) {
	t.Setenv("AJENT_HOME", t.TempDir())

	lines := []string{"hello", "/model", "@main.go", "hello"} // dedup keeps first
	Save(lines, "")
	got := Load("")
	assert.Equal(t, []string{"hello", "/model", "@main.go"}, got)
}

// TestLoadExcludesSecretPrefix verifies a secret-prefixed line is dropped on load.
func TestLoadExcludesSecretPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "history"),
		[]byte("good\nsecret:api-key\nalso-good\n"), 0o600))

	assert.Equal(t, []string{"good", "also-good"}, Load("secret:"))
}

// TestSaveDropsSecrets verifies a secret never reaches disk.
func TestSaveDropsSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)

	Save([]string{"good", "secret:api-key", "also-good"}, "secret:")
	raw, err := os.ReadFile(filepath.Join(home, "history"))
	require.NoError(t, err)
	assert.Contains(t, string(raw), "good")
	assert.NotContains(t, string(raw), "api-key", "a secret must never reach disk")
	assert.Equal(t, []string{"good", "also-good"}, Load("secret:"))
}

func TestLoadMissingReturnsNil(t *testing.T) {
	t.Setenv("AJENT_HOME", t.TempDir())
	assert.Nil(t, Load(""))
}

// TestSaveCapsToMax verifies the cap keeps the most recent lines.
func TestSaveCapsToMax(t *testing.T) {
	t.Setenv("AJENT_HOME", t.TempDir())
	lines := make([]string, MaxLines+50)
	for i := range lines {
		lines[i] = "line-" + strconv.Itoa(i)
	}
	Save(lines, "")
	got := Load("")
	assert.Len(t, got, MaxLines)
	assert.Equal(t, "line-"+strconv.Itoa(MaxLines+49), got[len(got)-1])
}

// TestLoadCapsHandEditedFile verifies load caps an over-long file at MaxLines,
// keeping the most recent.
func TestLoadCapsHandEditedFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AJENT_HOME", home)
	var b []byte
	for i := 0; i < MaxLines+10; i++ {
		b = append(b, []byte("x-"+strconv.Itoa(i)+"\n")...)
	}
	require.NoError(t, os.WriteFile(filepath.Join(home, "history"), b, 0o600))

	got := Load("")
	assert.Len(t, got, MaxLines)
	assert.Equal(t, "x-"+strconv.Itoa(MaxLines+9), got[len(got)-1])
}
