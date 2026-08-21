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

// TestEditPreview asserts an edit previews the post-edit text on the after side,
// so what is rendered is what the call would produce.
func TestEditPreview(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "hello world\n")
	c := agent.ToolCall{ID: "c", Name: "edit",
		Input: json.RawMessage(`{"path":"a.txt","edits":[{"oldText":"world","newText":"ajent"}]}`)}
	ch, err := (&editTool{policy: e.policy, tracker: e.tracker}).Preview(c)
	require.NoError(t, err)

	assert.Equal(t, "a.txt", ch.Path)
	assert.Equal(t, "hello world\n", ch.Before) // pre-edit text
	assert.Equal(t, "hello ajent\n", ch.After)  // post-edit text, not the same buffer twice
}

// TestApplyEditsValidation covers the order-independent validate pass shared by
// Execute and DryRun: malformed edits fail before any write.
func TestApplyEditsValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		content     string
		args        string
		errContains string // empty means the edit is accepted
	}{
		{"rejects_empty_old_text", "x\n", `[{"oldText":"","newText":"y"}]`, "empty oldText"},
		{"rejects_noop_edit", "x\n", `[{"oldText":"same","newText":"same"}]`, "no-op edit"}, // changes nothing
		{"rejects_duplicate_old_text", "one two\n", `[{"oldText":"one","newText":"1"},{"oldText":"one","newText":"2"}]`, "repeat the same oldText"},
		{"rejects_overlapping_regions", "abcdef\n", `[{"oldText":"bcd","newText":"X"},{"oldText":"cde","newText":"Y"}]`, "overlapping regions"},
		{"allows_adjacent_non_overlap", "abcdef\n", `[{"oldText":"ab","newText":"1"},{"oldText":"cd","newText":"2"}]`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newToolEnv(t.TempDir())
			e.writeFile("a.txt", tc.content)
			err := e.editDryRun(`{"path":"a.txt","edits":` + tc.args + `}`)
			if tc.errContains != "" {
				require.Error(t, err) // rejected for the named reason
				assert.Contains(t, err.Error(), tc.errContains)
				return
			}
			assert.NoError(t, err)
		})
	}
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

	data, rerr := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	require.NoError(t, rerr)
	assert.Equal(t, orig, string(data)) // dry run never writes
}

func TestEditAppliesSingleMatch(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("code.go", "var x = 1\n")
	_ = e.readExec(t.Context(), `{"path":"code.go"}`)

	res := e.editExec(t.Context(),
		`{"path":"code.go","edits":[{"oldText":"x = 1","newText":"y = 2"}]}`)
	assert.False(t, res.IsError)
	data, err := os.ReadFile(filepath.Join(e.cwd, "code.go"))
	require.NoError(t, err)
	assert.Equal(t, "var y = 2\n", string(data))
}

func TestEditZeroMatchIsActionable(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "the quick brown fox")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"quick brwn fx","newText":"slow"}]}`)
	assert.True(t, res.IsError)
	out := textOf(res)
	assert.Contains(t, out, "no match") // names the failure
}

func TestEditAmbiguousMatchRequiresReplaceAll(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "aaa bbb aaa\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"aaa","newText":"zzz"}]}`)
	assert.True(t, res.IsError) // two occurrences without replace_all
	out := textOf(res)
	assert.Contains(t, out, "2 occurrences")

	res = e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"aaa","newText":"zzz","replace_all":true}]}`)
	assert.False(t, res.IsError)
	data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "zzz bbb zzz\n", string(data))
}

func TestEditMultiAtomicRollbackOnFailure(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	orig := "one\ntwo\nthree\n"
	e.writeFile("a.txt", orig)
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"one","newText":"uno"},{"oldText":"missing","newText":"nope"}]}`)
	assert.True(t, res.IsError) // second edit's old text is missing; batch must not apply

	data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, orig, string(data)) // byte-identical: first edit rolled back
}

func TestEditAppliesAfterExternalChangeWhenMatchHolds(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "original\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	e.writeFile("a.txt", "kept line original\n") // changed externally, oldText still present

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"original","newText":"mine"}]}`)
	assert.False(t, res.IsError) // match validation governs, not a stale gate
}

func TestEditEmptyOldTextRejected(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "x\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"","newText":"y"}]}`)
	assert.True(t, res.IsError)
	assert.Contains(t, textOf(res), "empty oldText") // named, not silently skipped
}

func TestEditEmptyEditsRejected(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	e.writeFile("a.txt", "x\n")
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
	res := e.editExec(t.Context(), `{"path":"a.txt","edits":[]}`)
	assert.True(t, res.IsError)
}
