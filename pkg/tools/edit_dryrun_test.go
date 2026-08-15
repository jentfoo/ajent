package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (e *toolEnv) editDryRun(args string) error {
	c := agent.ToolCall{ID: "c", Name: "edit", Input: json.RawMessage(args)}
	return (&editTool{policy: e.policy, tracker: e.tracker}).DryRun(c)
}

// newEditRegistry builds a registry holding an edit tool bound to dir.
func newEditRegistry(dir string) (*Registry, *Tracker) {
	tr := NewTracker()
	reg := New()
	reg.Register(&editTool{policy: PathPolicy{Cwd: dir}, tracker: tr}, true)
	return reg, tr
}

func TestApplyEditsRejectsEmptyOldText(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "x\n")
	err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"","newText":"y"}]}`)
	assert.ErrorContains(t, err, "empty oldText")
}

func TestApplyEditsRejectsNoopEdit(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "x\n")
	err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"same","newText":"same"}]}`)
	assert.ErrorContains(t, err, "no-op edit") // old equals new changes nothing
}

func TestApplyEditsRejectsDuplicateOldText(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "one two\n")
	err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"one","newText":"1"},{"oldText":"one","newText":"2"}]}`)
	assert.ErrorContains(t, err, "repeat the same oldText") // ambiguous intent
}

func TestApplyEditsRejectsOverlappingRegions(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "abcdef\n")
	// both edits target shared bytes in the original text
	err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"bcd","newText":"X"},{"oldText":"cde","newText":"Y"}]}`)
	assert.ErrorContains(t, err, "overlapping regions")
}

func TestApplyEditsAllowsAdjacentNonOverlap(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "abcdef\n")
	// adjacent edits share no byte and are valid
	err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"ab","newText":"1"},{"oldText":"cd","newText":"2"}]}`)
	assert.NoError(t, err)
}

func TestEditDryRunMissingFileIsWillFail(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	err := e.editDryRun(`{"path":"nope.txt","edits":[{"oldText":"a","newText":"b"}]}`)
	assert.Error(t, err) // missing file counts as doomed; skip the prompt
}

func TestEditDryRunMutatesNothingOnDisk(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	orig := "one two three\n"
	e.writeFile("a.txt", orig)
	err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"missing","newText":"x"}]}`)
	require.Error(t, err)

	data, _ := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	assert.Equal(t, orig, string(data)) // dry run never writes
}

func TestRegistryDryRunDispatchesToTool(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	e := newToolEnv(dir)
	e.writeFile("a.txt", "hello\n")
	reg, _ := newEditRegistry(dir)

	c := agent.ToolCall{ID: "c", Name: "edit", Input: json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"missing","newText":"x"}]}`)}
	require.Error(t, reg.DryRun(c)) // edit implements DryRunner; a doomed call errors

	c.Input = json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"hello","newText":"hi"}]}`)
	require.NoError(t, reg.DryRun(c))
}

func TestRegistryDryRunNilForNonDryTool(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	reg := New()
	reg.Register(&readTool{policy: e.policy, tracker: e.tracker}, true)

	c := agent.ToolCall{ID: "c", Name: "read", Input: json.RawMessage(`{"path":"x"}`)}
	assert.NoError(t, reg.DryRun(c)) // cannot predict; never skip a prompt on uncertainty
}

func TestRegistryDryRunNilForUnknownTool(t *testing.T) {
	t.Parallel()

	reg := New()
	c := agent.ToolCall{ID: "c", Name: "nope", Input: json.RawMessage(`{}`)}
	assert.NoError(t, reg.DryRun(c))
}
