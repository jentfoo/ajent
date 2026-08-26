package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		{"rejects_overlapping_regions", "abcdef\n", `[{"oldText":"bcd","newText":"X"},{"oldText":"cde","newText":"Y"}]`, "target overlapping regions in a.txt"},
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

// TestEditFailure covers every failure path of the edit tool: pure oldText
// diagnosis (zero-match), and end-to-end rejection via Execute.
func TestEditFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		old, buf     string
		wantContains []string
		wantNot      []string // substrings that must not appear (no false blame)
	}{
		{"tab_vs_space_text_uses_spaces", "if x {\n foo()\n}\n", "if x {\n\tfoo()\n}\n",
			[]string{"must match it exactly"}, nil},
		{"file_tab_text_more_spaces", "if x {\n    foo()\n}\n", "if x {\n\tfoo()\n}\n",
			[]string{"1 tab(s)", "4 space(s)"}, nil},
		{"indent_count_differs", "if x {\n  foo()\n}\n", "if x {\n    foo()\n}\n", // text 2, file 4
			[]string{"indentation count differs"}, nil},
		{"no_file_indent", "\tfoo()", "foo()",
			[]string{"file line is not indented"}, []string{"use spaces"}}, // no false prescription
		{"content_differs", "no such function here", "the quick brown fox",
			[]string{"not in the file"}, []string{"whitespace"}},
		{"inter_word_spacing", "var  a = 1\n", "var a = 1\n",
			[]string{"the file has 1 space, your text has 2 spaces"}, nil},
		{"letter_case_differs", "Foo Bar", "foo bar",
			[]string{"letter case"}, []string{"whitespace"}},
		{"file_trailing_ws_omitted", "\nfoo", "foo \t",
			[]string{"the file line ends with mixed 1 tab and 1 space your text omits"}, nil},
		{"text_only_trailing_ws", "x = 1 \ny=2", "x = 1\ny=2",
			[]string{"your line ends with 1 space that is not in the file; remove it"}, nil},
		{"extra_blank_lines_in_text", "func foo() {\n\n\n  bar()\n}", "func foo() {\n\n  bar()\n}",
			[]string{"2 blank lines after line 1", "file has only 1 blank line there"}, nil},
		{"missing_blank_lines_in_text", "func foo() {\n  bar()\n}", "func foo() {\n\n  bar()\n}",
			[]string{"1 blank line after line 1 that your oldText omits"}, nil},
	}

	for _, tc := range cases {
		t.Run("zero_match_"+tc.name, func(t *testing.T) {
			issues := diagnoseNoMatch(tc.old, tc.buf)
			require.NotEmpty(t, issues)
			joined := strings.Join(issues, "\n")
			for _, w := range tc.wantContains {
				assert.Contains(t, joined, w)
			}
			for _, nw := range tc.wantNot {
				assert.NotContains(t, joined, nw) // no blanket claim on the wrong axis
			}
		})
	}

	t.Run("zero_match_is_actionable", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "the quick brown fox")
		_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

		res := e.editExec(t.Context(),
			`{"path":"a.txt","edits":[{"oldText":"quick brwn fx","newText":"slow"}]}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), "no match") // names the failure
	})

	t.Run("ambiguous_requires_replace_all", func(t *testing.T) {
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
	})

	t.Run("multi_atomic_rollback_on_failure", func(t *testing.T) {
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
	})

	t.Run("empty_old_text_rejected", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "x\n")
		_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
		res := e.editExec(t.Context(),
			`{"path":"a.txt","edits":[{"oldText":"","newText":"y"}]}`)
		assert.True(t, res.IsError)
		assert.Contains(t, textOf(res), "empty oldText") // named, not silently skipped
	})

	t.Run("empty_edits_rejected", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "x\n")
		_ = e.readExec(t.Context(), `{"path":"a.txt"}`)
		res := e.editExec(t.Context(), `{"path":"a.txt","edits":[]}`)
		assert.True(t, res.IsError)
	})
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

func TestEditPreservesExistingPerms(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	p := filepath.Join(e.cwd, "secret.txt")
	require.NoError(t, os.WriteFile(p, []byte("old\n"), 0o600))

	res := e.editExec(t.Context(), `{"path":"secret.txt","edits":[{"oldText":"old","newText":"new"}]}`)
	assert.False(t, res.IsError)
	fi, err := os.Stat(p)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm()) // owner-only mode kept
}

// TestEditCrlfFileMatchesLfOldTextAndWritesBackCrlf proves the model reads LF and
// writes match the document's existing CRLF, never mixing endings.
func TestEditCrlfPreservesLineEnding(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	p := filepath.Join(e.cwd, "a.txt")
	require.NoError(t, os.WriteFile(p, []byte("alpha\r\nbeta\r\n"), 0o644))
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"beta","newText":"gamma"}]}`)
	assert.False(t, res.IsError) // LF oldText matches the CRLF file

	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "alpha\r\ngamma\r\n", string(data)) // CRLF kept, no mixed endings
}

// TestEditMixedEndingsFollowNeighborhood pins the per-edit ending rule: a
// replacement adopts the ending of the line it starts on, and untouched regions
// keep their exact bytes, in a file that mixes CRLF and LF.
func TestEditMixedEndingsFollowNeighborhood(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	p := filepath.Join(e.cwd, "mix.txt")
	require.NoError(t, os.WriteFile(p, []byte("aaa\r\nbbb\n"), 0o644))
	_ = e.readExec(t.Context(), `{"path":"mix.txt"}`)

	// an edit on the LF line writes LF even though the file opens CRLF
	res := e.editExec(t.Context(),
		`{"path":"mix.txt","edits":[{"oldText":"bbb","newText":"b1\nb2"}]}`)
	assert.False(t, res.IsError)
	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "aaa\r\nb1\nb2\n", string(data))

	// an edit on the CRLF line writes CRLF; the untouched LF line stays LF
	require.NoError(t, os.WriteFile(p, []byte("aaa\r\nbbb\n"), 0o644))
	res = e.editExec(t.Context(),
		`{"path":"mix.txt","edits":[{"oldText":"aaa","newText":"a1\na2"}]}`)
	assert.False(t, res.IsError)
	data, err = os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "a1\r\na2\r\nbbb\n", string(data))
}

// TestEditCascadeFailsAgainstOriginal proves edit N+1 cannot match text edit N wrote.
func TestEditCascadeRejectedAndFileUntouched(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	p := filepath.Join(e.cwd, "a.txt")
	require.NoError(t, os.WriteFile(p, []byte("foo"), 0o644))
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"foo","newText":"bar"},{"oldText":"bar","newText":"baz"}]}`)
	assert.True(t, res.IsError) // second edit's oldText is not in the original
	out := textOf(res)
	assert.Contains(t, out, "edit 2") // names the failing op
	assert.Contains(t, out, "'bar'")  // names what it looked for

	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "foo", string(data)) // byte-identical: no cascade applied
}

// TestEditPiNonCascadeSucceeds pins pi's case that ajent failed before span
// resolution: edit 1 creates a second copy of the text edit 2 targets.
func TestEditPiNonCascadeCase(t *testing.T) {
	t.Parallel()

	e := newToolEnv(t.TempDir())
	p := filepath.Join(e.cwd, "a.txt")
	require.NoError(t, os.WriteFile(p, []byte("foo\nbar\nbaz\n"), 0o644))
	_ = e.readExec(t.Context(), `{"path":"a.txt"}`)

	res := e.editExec(t.Context(),
		`{"path":"a.txt","edits":[{"oldText":"foo\n","newText":"foo bar\n"},{"oldText":"bar\n","newText":"BAR\n"}]}`)
	assert.False(t, res.IsError) // both resolve against the original buffer

	data, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, "foo bar\nBAR\nbaz\n", string(data))
}
