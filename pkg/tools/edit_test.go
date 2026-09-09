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

func TestDecodeEditParams(t *testing.T) {
	t.Parallel()

	want := editParams{Path: "a.txt", Edits: []editOp{{OldText: "a", NewText: "b"}}}

	cases := []struct {
		name string
		args string
	}{
		{"declared_shape", `{"path":"a.txt","edits":[{"oldText":"a","newText":"b"}]}`},
		{"edits_as_json_string", `{"path":"a.txt","edits":"[{\"oldText\":\"a\",\"newText\":\"b\"}]"}`},
		{"single_edit_object", `{"path":"a.txt","edits":{"oldText":"a","newText":"b"}}`},
		{"single_edit_as_string", `{"path":"a.txt","edits":"{\"oldText\":\"a\",\"newText\":\"b\"}"}`},
		{"double_encoded_arguments", `"{\"path\":\"a.txt\",\"edits\":[{\"oldText\":\"a\",\"newText\":\"b\"}]}"`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeEditParams(json.RawMessage(tc.args))
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}

	t.Run("replace_all_survives", func(t *testing.T) {
		got, err := decodeEditParams(json.RawMessage(
			`{"path":"a.txt","edits":{"oldText":"a","newText":"b","replace_all":true}}`))
		require.NoError(t, err)
		assert.True(t, got.Edits[0].ReplaceAll)
	})

	t.Run("missing_edits_tolerated", func(t *testing.T) {
		got, err := decodeEditParams(json.RawMessage(`{"path":"a.txt"}`))
		require.NoError(t, err) // the empty-edits check reports it with a better message
		assert.Empty(t, got.Edits)
	})

	t.Run("malformed_still_rejected", func(t *testing.T) {
		_, err := decodeEditParams(json.RawMessage(`{"path":"a.txt","edits":[{`))
		assert.Error(t, err)
	})
}

func TestEditToleratesArgumentShape(t *testing.T) {
	t.Parallel()

	for _, args := range []string{
		`{"path":"a.txt","edits":{"oldText":"world","newText":"ajent"}}`,
		`{"path":"a.txt","edits":"[{\"oldText\":\"world\",\"newText\":\"ajent\"}]"}`,
	} {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.txt", "hello world\n")
		res := e.editExec(t.Context(), args)
		require.False(t, res.IsError, textOf(res))

		data, err := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
		require.NoError(t, err)
		assert.Equal(t, "hello ajent\n", string(data))
	}
}

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

func TestApplyEditsValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		content     string
		args        string
		errContains string // empty means the edit is accepted
	}{
		{"rejects_empty_edits_list", "x\n", `[]`, "at least one entry"}, // doomed, so never prompts
		{"rejects_empty_old_text", "x\n", `[{"oldText":"","newText":"y"}]`, "empty oldText"},
		{"rejects_noop_when_present", "same\n", `[{"oldText":"same","newText":"same"}]`, "identical"}, // changes nothing
		// absent oldText is a match failure, not a no-op; the model needs the file's text
		{"noop_absent_reports_no_match", "x\n", `[{"oldText":"same","newText":"same"}]`, "no match for edit 1"},
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

func TestEditDryRun(t *testing.T) {
	t.Parallel()

	// a missing file counts as doomed so the prompt is skipped
	t.Run("missing_file_is_will_fail", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		err := e.editDryRun(`{"path":"nope.txt","edits":[{"oldText":"a","newText":"b"}]}`)
		assert.Error(t, err) // missing file counts as doomed; skip the prompt
	})

	// a dry run never writes to disk
	t.Run("mutates_nothing_on_disk", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		orig := "one two three\n"
		e.writeFile("a.txt", orig)
		err := e.editDryRun(`{"path":"a.txt","edits":[{"oldText":"missing","newText":"x"}]}`)
		require.Error(t, err)

		data, rerr := os.ReadFile(filepath.Join(e.cwd, "a.txt"))
		require.NoError(t, rerr)
		assert.Equal(t, orig, string(data)) // dry run never writes
	})
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

func TestEditFailure(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		old, buf     string
		wantContains []string
		wantNot      []string // substrings that must not appear (no false blame)
	}{
		{"tab_vs_space_text_uses_spaces", "if x {\n foo()\n}\n", "if x {\n\tfoo()\n}\n",
			[]string{"the file line uses 1 tab", "your text has 1 space"}, nil},
		{"file_tab_text_more_spaces", "if x {\n    foo()\n}\n", "if x {\n\tfoo()\n}\n",
			[]string{"the file line uses 1 tab", "your text has 4 spaces"}, nil},
		{"indent_count_differs", "if x {\n  foo()\n}\n", "if x {\n    foo()\n}\n", // text 2, file 4
			[]string{"indentation differs", "the file line uses 4 spaces", "your text has 2 spaces"}, nil},
		{"no_file_indent", "\tfoo()", "foo()",
			[]string{"the file line uses no indentation", "your text has 1 tab"},
			[]string{"use spaces"}}, // no false prescription
		{"content_differs", "no such function here", "the quick brown fox",
			[]string{"appears nowhere in this file"}, []string{"whitespace"}},
		{"inter_word_spacing", "var  a = 1\n", "var a = 1\n",
			[]string{"the file has 1 space, your text has 2 spaces"}, nil},
		{"letter_case_differs", "Foo Bar", "foo bar",
			[]string{"letter case"}, []string{"whitespace"}},
		{"extra_blank_lines_in_text", "func foo() {\n\n\n  bar()\n}", "func foo() {\n\n  bar()\n}",
			[]string{"2 blank lines after line 1", "file has only 1 blank line there"}, nil},
		{"missing_blank_lines_in_text", "func foo() {\n  bar()\n}", "func foo() {\n\n  bar()\n}",
			[]string{"1 blank line after line 1 that your oldText omits"}, nil},
		{"tab_depth_differs", "if x {\n\t\tfoo()\n}\n", "if x {\n\t\t\tfoo()\n}\n",
			[]string{"the file line uses 3 tabs", "your text has 2 tabs"}, nil},
	}

	for _, tc := range cases {
		t.Run("zero_match_"+tc.name, func(t *testing.T) {
			// drive the production path: it canonicalizes first, so a diagnostic
			// reachable only from raw text cannot pass here
			msg := missingError(1, editTarget{Path: "a.go"}, tc.old, tc.buf, nil)
			for _, w := range tc.wantContains {
				assert.Contains(t, msg, w)
			}
			for _, nw := range tc.wantNot {
				assert.NotContains(t, msg, nw) // no blanket claim on the wrong axis
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

	// the model sent its newText in both fields: the message must name the
	// duplication and hand back the file's text, since no tier will heal it
	t.Run("duplicated_new_text_is_actionable", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("a.go", "func f() {\n\ttimeout := 30 * time.Second\n}\n")
		_ = e.readExec(t.Context(), `{"path":"a.go"}`)

		want := `\ttimeout := 60 * time.Second`
		res := e.editExec(t.Context(),
			`{"path":"a.go","edits":[{"oldText":"`+want+`","newText":"`+want+`"}]}`)
		require.True(t, res.IsError)
		out := textOf(res)
		assert.Contains(t, out, "nothing changes")
		assert.Contains(t, out, `you wrote "60" where the file has "30"`)
		assert.Contains(t, out, "\ttimeout := 30 * time.Second") // verbatim, copyable

		res = e.editExec(t.Context(), // the retry the message asks for
			`{"path":"a.go","edits":[{"oldText":"\ttimeout := 30 * time.Second","newText":"`+want+`"}]}`)
		assert.False(t, res.IsError)
		data, err := os.ReadFile(filepath.Join(e.cwd, "a.go"))
		require.NoError(t, err)
		assert.Equal(t, "func f() {\n\ttimeout := 60 * time.Second\n}\n", string(data))
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

// TestEditGuardsEarlierEditsAfterLinesMove covers the fuzzy guard's line ranges
// surviving an edit that moved them. Unremapped they name unrelated lines, and a
// heal then silently reverts work this session already did.
func TestEditGuardsEarlierEditsAfterLinesMove(t *testing.T) {
	t.Parallel()

	const src = "package p\n\nconst (\n\tqueueDepth      = 1024\n\tbatchSize       = 512\n)\n"
	const bump = `{"path":"c.go","edits":[{"oldText":"queueDepth      = 1024","newText":"queueDepth      = 4096"}]}`
	const heal = `{"path":"c.go","edits":[{"oldText":"queueDepth      = 256","newText":"queueDepth      = 8192"}]}`

	t.Run("refuses_after_an_insert_above", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("c.go", src)
		require.False(t, e.editExec(t.Context(), bump).IsError)

		res := e.editExec(t.Context(), `{"path":"c.go","edits":[{"oldText":"package p\n",`+
			`"newText":"package p\n\nimport \"x\"\nimport \"y\"\nimport \"z\"\n"}]}`)
		require.False(t, res.IsError, textOf(res))

		res = e.editExec(t.Context(), heal)
		assert.True(t, res.IsError) // moved, but still a line this session wrote

		data, err := os.ReadFile(filepath.Join(e.cwd, "c.go"))
		require.NoError(t, err)
		assert.Contains(t, string(data), "queueDepth      = 4096") // the first edit stands
	})

	t.Run("a_write_drops_the_ranges", func(t *testing.T) {
		e := newToolEnv(t.TempDir())
		e.writeFile("c.go", src)
		require.False(t, e.editExec(t.Context(), bump).IsError)

		// the model supplied the whole file, so nothing left is work to protect
		res := e.writeExec(t.Context(),
			`{"path":"c.go","content":"package p\n\nconst (\n\tqueueDepth      = 4096\n)\n"}`)
		require.False(t, res.IsError, textOf(res))

		res = e.editExec(t.Context(), heal)
		assert.False(t, res.IsError, textOf(res))
	})
}
