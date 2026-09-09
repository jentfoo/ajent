package tools

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonical(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"ascii_unchanged", "func f() {\n\treturn nil\n}\n", "func f() {\n\treturn nil\n}\n"},
		{"smart_quotes", "say “hi” and ‘bye’", `say "hi" and 'bye'`},
		{"dashes", "a — b – c − d", "a - b - c - d"},
		{"nbsp_and_wide_spaces", "a\u00a0b\u2009c\u3000d", "a b c d"},
		{"zero_width_dropped", "a\u200bb\ufeffc", "abc"},
		{"trailing_space_dropped", "x = 1   \ny = 2\t\n", "x = 1\ny = 2\n"},
		{"trailing_at_eof_dropped", "last line   ", "last line"},
		{"blank_line_emptied", "a\n   \nb", "a\n\nb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, pos := canonical(tc.in)
			assert.Equal(t, tc.want, got)
			require.Len(t, pos, len(got)+1)            // one entry per byte plus the end
			assert.Equal(t, len(tc.in), pos[len(got)]) // the end always maps to the end
			for i := 1; i < len(pos); i++ {
				assert.LessOrEqual(t, pos[i-1], pos[i]) // the map never runs backwards
			}
		})
	}
}

func TestFindMatchesTiers(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		buf, old string
		new      string // defaults to "REPL"; indent cases must share oldText's base
		wantTier matchTier
		wantText string // the file text the match covers; empty means no match
	}{
		{
			name: "exact", buf: "var x = 1\n", old: "x = 1",
			wantTier: tierExact, wantText: "x = 1",
		},
		{
			name: "smart_quotes", buf: "msg := \"hello\"\n", old: "msg := “hello”",
			wantTier: tierCanon, wantText: "msg := \"hello\"",
		},
		{
			name: "nbsp_for_space", buf: "a := b\n", old: "a\u00a0:=\u00a0b",
			wantTier: tierCanon, wantText: "a := b",
		},
		{
			// the file's trailing run falls inside the span so nothing is stranded
			name: "file_trailing_whitespace", buf: "x = 1   \ny = 2\n", old: "x = 1\ny = 2",
			wantTier: tierCanon, wantText: "x = 1   \ny = 2",
		},
		{
			name: "text_trailing_whitespace", buf: "x = 1\ny = 2\n", old: "x = 1   \ny = 2",
			wantTier: tierCanon, wantText: "x = 1\ny = 2",
		},
		{
			name:     "indent_shift_added",
			buf:      "func f() {\n    if err != nil {\n        return err\n    }\n}\n",
			old:      "if err != nil {\n    return err\n}",
			wantTier: tierIndent, wantText: "    if err != nil {\n        return err\n    }",
		},
		{
			name:     "indent_shift_reduced",
			buf:      "\tfoo()\n\tbar()\n",
			old:      "\t\tfoo()\n\t\tbar()",
			new:      "\t\tfoo()\n\t\tbaz()",
			wantTier: tierIndent, wantText: "\tfoo()\n\tbar()",
		},
		{
			// nested tabs vs spaces is not a uniform shift; refuse and let the
			// diagnostics explain it rather than reformat the block
			name: "nested_tab_space_refused",
			buf:  "func f() {\n\tif x {\n\t\ty()\n\t}\n}\n",
			old:  "if x {\n    y()\n}",
		},
		{name: "absent", buf: "the quick brown fox\n", old: "no such text"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repl := tc.new
			if repl == "" {
				repl = "REPL"
			}
			ms, tier := findMatches(tc.buf, tc.old, repl, nil)
			if tc.wantText == "" {
				assert.Empty(t, ms)
				return
			}
			require.Len(t, ms, 1)
			assert.Equal(t, tc.wantTier, tier)
			assert.Equal(t, tc.wantText, tc.buf[ms[0].s:ms[0].e])
		})
	}
}

func TestFindMatchesIndentReplacement(t *testing.T) {
	t.Parallel()

	buf := "func f() {\n    if err != nil {\n        return err\n    }\n}\n"
	ms, tier := findMatches(buf,
		"if err != nil {\n    return err\n}",
		"if err != nil {\n    return wrap(err)\n}", nil)
	require.Len(t, ms, 1)
	assert.Equal(t, tierIndent, tier)
	assert.Equal(t, "    if err != nil {\n        return wrap(err)\n    }", ms[0].repl)
}

func TestFindMatchesIndentRefusesUnsharedBase(t *testing.T) {
	t.Parallel()

	buf := "func f() {\n    a()\n    b()\n}\n"
	ms, _ := findMatches(buf, "  a()\n  b()", "  a()\nb()", nil)
	assert.Empty(t, ms)
}

func TestFindMatchesTierOrder(t *testing.T) {
	t.Parallel()

	buf := "value\n“value”\n"
	ms, tier := findMatches(buf, "value", "X", nil)
	assert.Equal(t, tierExact, tier)
	require.Len(t, ms, 2) // the bare line and the copy inside the quotes
}

func TestFindMatchesAmbiguousFuzzy(t *testing.T) {
	t.Parallel()

	buf := "a := \"x\"\nb := \"x\"\n"
	ms, tier := findMatches(buf, ":= “x”", "X", nil)
	assert.Equal(t, tierCanon, tier)
	assert.Len(t, ms, 2)
}

func TestFuzzyApplyMatchesExactApply(t *testing.T) {
	t.Parallel()

	const file = "package main\n\nfunc greet() string {\n" +
		"    msg := \"hello\"\n" +
		"    if msg == \"\" {\n" +
		"        return \"empty\"\n" +
		"    }\n" +
		"    return msg\n" +
		"}\n"

	exact := "    if msg == \"\" {\n        return \"empty\"\n    }"
	replacement := "    if msg == \"\" {\n        return fallback\n    }"

	// each case is a perturbation of exact paired with the replacement a model
	// sending that perturbation would write; every one must land on the same span
	// and produce the same file
	dedented := "if msg == \"\" {\n    return fallback\n}"
	cases := []struct {
		name     string
		old, new string
	}{
		{"identical", exact, replacement},
		{"smart_quotes", "    if msg == \u201c\u201d {\n        return \u201cempty\u201d\n    }", replacement},
		{"trailing_spaces", "    if msg == \"\" {   \n        return \"empty\"\t\n    }", replacement},
		{"nbsp_separators", "    if msg\u00a0== \"\" {\n        return \"empty\"\n    }", replacement},
		{"indent_shifted", "if msg == \"\" {\n    return \"empty\"\n}", dedented},
		{"combined", "if msg == \u201c\u201d {   \n    return \u201cempty\u201d\n}", dedented},
	}

	want, err := applyEdits(editTarget{Path: "a.go"}, file, []editOp{{OldText: exact, NewText: replacement}})
	require.NoError(t, err)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := applyEdits(editTarget{Path: "a.go"}, file, []editOp{{OldText: tc.old, NewText: tc.new}})
			require.NoError(t, err)
			assert.Equal(t, string(want.final), string(got.final))
		})
	}
}

func TestFuzzyPreservesLineEndings(t *testing.T) {
	t.Parallel()

	o, err := applyEdits(editTarget{Path: "a.txt"}, "alpha\r\nbeta\r\ngamma\r\n",
		[]editOp{{OldText: "alpha   \nbeta", NewText: "ALPHA\nBETA"}})
	require.NoError(t, err)
	assert.Equal(t, "ALPHA\r\nBETA\r\ngamma\r\n", string(o.final))
}

func TestFuzzyNoteNamesTheDifference(t *testing.T) {
	t.Parallel()

	quotes, err := applyEdits(editTarget{Path: "a.go"}, "msg := \"hi\"\n",
		[]editOp{{OldText: "msg := “hi”", NewText: "msg := \"bye\""}})
	require.NoError(t, err)
	require.Len(t, quotes.notes, 1)
	assert.Contains(t, quotes.notes[0], "edit 1")
	assert.Contains(t, quotes.notes[0], "unicode characters")
	assert.Contains(t, quotes.notes[0], `“hi”`) // the note names the characters that differed

	nbsp, err := applyEdits(editTarget{Path: "a.go"}, "a := b\n",
		[]editOp{{OldText: "a\u00a0:=\u00a0b", NewText: "a := c"}})
	require.NoError(t, err)
	require.Len(t, nbsp.notes, 1)
	assert.Contains(t, nbsp.notes[0], "unicode characters")
	assert.NotContains(t, nbsp.notes[0], "trailing whitespace")

	trailing, err := applyEdits(editTarget{Path: "a.go"}, "x = 1   \ny = 2\n",
		[]editOp{{OldText: "x = 1\ny = 2", NewText: "x = 9\ny = 2"}})
	require.NoError(t, err)
	require.Len(t, trailing.notes, 1)
	assert.Contains(t, trailing.notes[0], "trailing whitespace")

	indent, err := applyEdits(editTarget{Path: "a.go"}, "func f() {\n\tcall()\n}\n",
		[]editOp{{OldText: "call()", NewText: "call(ctx)"}})
	require.NoError(t, err)
	assert.Empty(t, indent.notes) // a bare substring matches exactly; no shift involved

	shifted, err := applyEdits(editTarget{Path: "a.go"}, "func f() {\n\ta()\n\tb()\n}\n",
		[]editOp{{OldText: "a()\nb()", NewText: "a()\nc()"}})
	require.NoError(t, err)
	require.Len(t, shifted.notes, 1)
	assert.Contains(t, shifted.notes[0], "shifting indentation")
	assert.Contains(t, shifted.notes[0], "1 tab")
	assert.Equal(t, "func f() {\n\ta()\n\tc()\n}\n", string(shifted.final))
}

func TestFuzzyDoesNotMaskDiagnostics(t *testing.T) {
	t.Parallel()

	_, err := applyEdits(editTarget{Path: "a.go"}, "func f() {\n\tif x {\n\t\ty()\n\t}\n}\n",
		[]editOp{{OldText: "if x {\n    y()\n}", NewText: "if x {\n    z()\n}"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no match for edit 1")
	assert.Contains(t, err.Error(), "tab") // the indentation diagnostic still fires
}

func TestFindMatchesRefusesToFoldTheEdit(t *testing.T) {
	t.Parallel()

	t.Run("dash_near_miss_refused", func(t *testing.T) {
		// the file already holds the hyphen here; the em dash the model quoted is
		// somewhere else, so this must fail rather than report a no-op success
		ms, _ := findMatches("title - subtitle\n", "title — subtitle", "title - subtitle", nil)
		assert.Empty(t, ms)
	})

	t.Run("real_near_miss_matches", func(t *testing.T) {
		ms, tier := findMatches("msg := \"hi\"\n", "msg := “hi”", "msg := \"bye\"", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierCanon, tier)
	})

	t.Run("self_replace_refuses_indent", func(t *testing.T) {
		// the site's trailing whitespace makes the indent tier's replacement
		// byte-different, so it would apply as a success while asking for nothing
		ms, _ := findMatches("\tfoo bar  \n", "  foo bar", "  foo bar", nil)
		assert.Empty(t, ms)
	})

	t.Run("self_replace_refuses_fuzzy", func(t *testing.T) {
		// healing here cannot tell a duplicated newText from a mis-transcribed oldText
		ms, _ := findMatches("timeout := 30 * time.Second\n",
			"timeout := 60 * time.Second", "timeout := 60 * time.Second", nil)
		assert.Empty(t, ms)
	})

	t.Run("indent_tier_stays_reachable", func(t *testing.T) {
		// the guard skips the canon tier, but a block quoted one level out is
		// still a provable match and must not be lost with it
		buf := "\tif x {\n\t\tfoo — bar\n\t}\n"
		ms, tier := findMatches(buf, "if x {\n\tfoo — bar\n}", "if x {\n\tfoo - bar\n}", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierIndent, tier)
		assert.Equal(t, "\tif x {\n\t\tfoo - bar\n\t}", ms[0].repl) // the file's indent is kept
	})
}

func TestIndentMatchesColumnZero(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		buf, old, new string
		wantRepl      string
	}{
		{
			name: "spaces_onto_flush_left",
			buf:  "a := 1\nb := 2\n",
			old:  "  a := 1\n  b := 2", new: "  a := 1\n  b := 3",
			wantRepl: "a := 1\nb := 3",
		},
		{
			name: "tabs_onto_flush_left",
			buf:  "var x = 1\nvar y = 2\n",
			old:  "\tvar x = 1\n\tvar y = 2", new: "\tvar x = 1\n\tvar y = 3",
			wantRepl: "var x = 1\nvar y = 3",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms, tier := findMatches(tc.buf, tc.old, tc.new, nil)
			require.Len(t, ms, 1)
			assert.Equal(t, tierIndent, tier)
			assert.Equal(t, tc.wantRepl, ms[0].repl)
		})
	}

	t.Run("flush_left_is_ambiguous", func(t *testing.T) {
		// picking one of these silently is the failure the canonical base hid
		buf := "\nfoo()\nbar()\nfoo()\n"
		ms, _ := findMatches(buf, "  foo()", "  foo(ctx)", nil)
		assert.Len(t, ms, 2) // reported, so applyEdits can refuse
	})
}

func TestMatchesKeepLineCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		buf, old, new string
	}{
		{"blank_first_line", "\n  foo(1024)\n  bar\n", "\n  foo(256)\n  bar", "\n  foo(512)\n  bar"},
		{"blank_line_above_block", "   \n   \n  baz\n", "    baz", "    baz//Z"},
		{"whitespace_only_line_above", "   \n}\n", "  }", "  }//Z"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms, _ := findMatches(tc.buf, tc.old, tc.new, nil)
			for _, m := range ms {
				assert.Equal(t, strings.Count(tc.new, "\n"), strings.Count(m.repl, "\n"))
			}
			o, err := applyEdits(editTarget{Path: "a.go"}, tc.buf, []editOp{{OldText: tc.old, NewText: tc.new}})
			if err == nil {
				assert.Equal(t, strings.Count(tc.buf, "\n"), strings.Count(o.after, "\n"))
			}
		})
	}
}

func TestPreserveQuoted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		buf, old, new string
		want          string
	}{
		{
			name: "curly_quote_outside_the_change",
			buf:  "msg := \"don’t\"\n",
			old:  "msg := \"don't\"", new: "warn := \"don't\"",
			want: "warn := \"don’t\"\n",
		},
		{
			name: "nbsp_outside_the_change",
			buf:  "a = 1\n",
			old:  "a = 1", new: "a = 2",
			want: "a = 2\n",
		},
		{
			name: "file_trailing_whitespace_kept",
			buf:  "x = 1   \ny = 2\n",
			old:  "x = 1\ny = 2", new: "x = 1\ny = 3",
			want: "x = 1   \ny = 3\n",
		},
		{
			name: "em_dash_on_a_quoted_line",
			buf:  "title — sub\nn = 1\n",
			old:  "title - sub\nn = 1", new: "title - sub\nn = 2",
			want: "title — sub\nn = 2\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, err := applyEdits(editTarget{Path: "a.go"}, tc.buf,
				[]editOp{{OldText: tc.old, NewText: tc.new}})
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(o.final))
		})
	}

	t.Run("a_deliberate_fold_still_applies", func(t *testing.T) {
		// here the dash is the edit, not quoted context, so newText wins whole
		o, err := applyEdits(editTarget{Path: "a.go"}, "\tif x {\n\t\tfoo — bar\n\t}\n",
			[]editOp{{OldText: "if x {\n\tfoo — bar\n}", NewText: "if x {\n\tfoo - bar\n}"}})
		require.NoError(t, err)
		assert.Equal(t, "\tif x {\n\t\tfoo - bar\n\t}\n", string(o.final))
	})
}

func TestFindMatchesRefusesBlankOldText(t *testing.T) {
	t.Parallel()

	ms, _ := findMatches("a\n\nb\n", "   \n  ", "   \n  //Z", nil)
	assert.Empty(t, ms)

	ms, tier := findMatches("a\n   \nb\n", "   ", "xx", nil) // exact still applies
	require.Len(t, ms, 1)
	assert.Equal(t, tierExact, tier)
}

func TestShiftRanges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		prior  []lineRange
		shifts []lineShift
		want   []lineRange
	}{
		{"no_shifts", []lineRange{{2, 3}}, nil, []lineRange{{2, 3}}},
		{"insert_above", []lineRange{{2, 3}}, []lineShift{{at: 1, delta: 3}}, []lineRange{{5, 6}}},
		{"delete_above", []lineRange{{5, 6}}, []lineShift{{at: 1, delta: -2}}, []lineRange{{3, 4}}},
		{"change_below_does_not_move", []lineRange{{2, 3}}, []lineShift{{at: 8, delta: 4}}, []lineRange{{2, 3}}},
		{"two_shifts_accumulate", []lineRange{{9, 10}},
			[]lineShift{{at: 1, delta: 2}, {at: 4, delta: 1}}, []lineRange{{12, 13}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shiftRanges(tc.prior, tc.shifts))
		})
	}
}

func TestFuzzyMatches(t *testing.T) {
	t.Parallel()

	const file = "package p\n\nconst (\n\tqueueDepth      = 1024\n\tbatchSize       = 512\n)\n"

	t.Run("heals_a_mistyped_value", func(t *testing.T) {
		// the model dropped the leading tab as well as mistyping the value; both
		// heal in one pass and the file's indentation is kept
		ms, tier := findMatches(file, "queueDepth      = 256", "queueDepth      = 4096", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierFuzzy, tier)
		assert.Equal(t, "\tqueueDepth      = 4096", ms[0].repl)
		assert.Equal(t, "\tqueueDepth      = 1024", file[ms[0].s:ms[0].e])
	})

	t.Run("heals_value_and_indent_together", func(t *testing.T) {
		src := "func f() {\n\tif n > 1024 {\n\t\treturn\n\t}\n}\n"
		ms, tier := findMatches(src, "if n > 256 {\n\treturn\n}", "if n > 4096 {\n\treturn\n}", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierFuzzy, tier)
		assert.Equal(t, "\tif n > 4096 {\n\t\treturn\n\t}", ms[0].repl)
	})

	t.Run("run_over_limit_refused", func(t *testing.T) {
		ms, _ := findMatches(file, "queueDepth      = 7654321", "queueDepth      = 4096", nil)
		assert.Empty(t, ms)
	})

	t.Run("refuses_two_qualifying_regions", func(t *testing.T) {
		two := "a = 1024\nb = 1024\n"
		ms, _ := findMatches(two, "= 1020", "= 4096", nil)
		assert.Empty(t, ms) // choosing between them would be a guess
	})

	t.Run("heals_every_line_in_block", func(t *testing.T) {
		// the per-line budget is what the single-span rule could not express: two
		// mistyped values in one block, each inside the part the edit rewrites
		src := "cfg := Limits{\n\twindow:  32000,\n\treserve: 6400,\n}\n"
		ms, tier := findMatches(src,
			"cfg := Limits{\n\twindow:  32100,\n\treserve: 6500,\n}",
			"cfg := Limits{\n\twindow:  64000,\n\treserve: 9600,\n}", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierFuzzy, tier)
		assert.Equal(t, "cfg := Limits{\n\twindow:  64000,\n\treserve: 9600,\n}", ms[0].repl)
	})

	t.Run("refuses_context_line_drift", func(t *testing.T) {
		// window is context here: the model mis-copied a line it is not changing,
		// so applying its newText would silently rewrite 32000 to 32100
		src := "cfg := Limits{\n\twindow:  32000,\n\treserve: 6400,\n}\n"
		ms, _ := findMatches(src,
			"cfg := Limits{\n\twindow:  32100,\n\treserve: 6400,\n}",
			"cfg := Limits{\n\twindow:  32100,\n\treserve: 9600,\n}", nil)
		assert.Empty(t, ms)
	})

	t.Run("refuses_diff_outside_span", func(t *testing.T) {
		// the model retypes the value wrongly but means to change the name, so
		// writing its newText would silently revert 1024 to 256
		ms, _ := findMatches(file, "queueDepth      = 256", "queueSize       = 256", nil)
		assert.Empty(t, ms)
	})

	t.Run("refuses_previously_edited_line", func(t *testing.T) {
		edited := []lineRange{{from: 3, to: 4}} // the queueDepth line
		ms, _ := findMatches(file, "queueDepth      = 256", "queueDepth      = 4096", edited)
		assert.Empty(t, ms)
	})

	t.Run("refuses_without_anchoring_context", func(t *testing.T) {
		// "bar" and "foo" share nothing; with no context either side, any short
		// string would heal onto any other
		ms, _ := findMatches("foo", "bar", "baz", nil)
		assert.Empty(t, ms)
	})

	t.Run("refuses_unequal_line_counts", func(t *testing.T) {
		// the shape both observed build breaks came through: newText adds a closing
		// brace, so no line pairs up and the drift cannot be held to what it rewrites
		src := "func readPolicy() policy {\n\treturn policy{\n\t\tattempts: 5,\n" +
			"\t\tbackoff:  1500,\n\t\tjitter:   250,\n\t}\n}\n"
		ms, _ := findMatches(src,
			"func readPolicy() policy {\n\treturn policy{\n\t\tattempts: 5,\n"+
				"\t\tbackoff:  2500,\n\t\tjitter:   400,\n\t}",
			"func readPolicy() policy {\n\treturn policy{\n\t\tattempts: 5,\n"+
				"\t\tbackoff:  1500,\n\t\tjitter:   500,\n\t}\n}", nil)
		assert.Empty(t, ms)
	})

	t.Run("caps_drifting_lines", func(t *testing.T) {
		block := func(v string, n int) string {
			ls := make([]string, n)
			for i := range ls {
				ls[i] = fmt.Sprintf("\tk%d: %s,", i, v)
			}
			return strings.Join(ls, "\n")
		}
		for _, tc := range []struct {
			name  string
			lines int
			want  int
		}{
			{"at_the_cap", fuzzyDriftLines, 1},
			{"over_the_cap", fuzzyDriftLines + 1, 0},
		} {
			t.Run(tc.name, func(t *testing.T) {
				src := "cfg := M{\n" + block("100", tc.lines) + "\n}\n"
				ms, _ := findMatches(src, block("200", tc.lines), block("300", tc.lines), nil)
				assert.Len(t, ms, tc.want)
			})
		}
	})

	t.Run("refuses_close_rival_match", func(t *testing.T) {
		// winner drifts 4, rival 5: too close to call, so neither is written
		ms, _ := findMatches("x = 5678\nx = 56789\n", "x = 1234", "x = 9999", nil)
		assert.Empty(t, ms)
	})

	t.Run("matches_with_large_margin", func(t *testing.T) {
		// winner drifts 1 against a rival at 5; the margin must not touch this
		ms, tier := findMatches("x = 1244\nx = 56789\n", "x = 1234", "x = 9999", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierFuzzy, tier)
		assert.Equal(t, "x = 9999", ms[0].repl)
	})

	t.Run("refuses_noop_heal", func(t *testing.T) {
		// a heal writing nothing still reports success; catch it before
		// the model believes the correction landed
		src := "func slug(s string) string {\n\tif len(s) > 40 {\n\t\ts = s[:40]\n\t}\n\treturn s\n}\n"
		_, err := applyEdits(editTarget{Path: "a.go"}, src,
			[]editOp{{OldText: "\tif len(s) > 20 {\n\t\ts = s[:20]\n\t}",
				NewText: "\tif len(s) > 40 {\n\t\ts = s[:40]\n\t}"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "would change nothing")
	})

	t.Run("refuses_inconsistent_base", func(t *testing.T) {
		// the closing brace sits below the first line's base indent, so no
		// uniform shift exists and the short line must never be sliced
		src := "func f() {\n\tif x {\n\t\tdo()\n\t}\n}\n"
		ms, _ := findMatches(src, "\t\tif x {\n\t\t\tdo()\n}", "\t\tif x {\n\t\t\tdone()\n}", nil)
		assert.Empty(t, ms)
	})

	t.Run("preserves_prefix_over_slice", func(t *testing.T) {
		// the replacement's indent is a lookalike of oldText's base; swapping
		// the prefix would cut mid-rune, so the line keeps its own bytes
		o, err := applyEdits(editTarget{Path: "a.go"}, "\tcall(9234)\n",
			[]editOp{{OldText: " call(1234)", NewText: "\u00a0call(5678)"}})
		require.NoError(t, err)
		assert.Equal(t, "\u00a0call(5678)\n", string(o.final))
	})

	t.Run("leaves_an_exact_match_alone", func(t *testing.T) {
		ms, tier := findMatches(file, "batchSize       = 512", "batchSize       = 256", nil)
		require.Len(t, ms, 1)
		assert.Equal(t, tierExact, tier)
	})
}

func TestDriftRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		a, b     string
		wantA    string
		wantB    string
		wantNone bool
	}{
		{name: "shared_trailing_digit", a: "payloadBytes = 4096", b: "payloadBytes = 65536",
			wantA: "4096", wantB: "65536"},
		{name: "shared_leading_digit", a: "ttlMs:   30000", b: "ttlMs:   60000",
			wantA: "30000", wantB: "60000"},
		{name: "both_edges_shared", a: "x = 8300000", b: "x = 8388608",
			wantA: "8300000", wantB: "8388608"},
		{name: "already_whole", a: "x = 256", b: "x = 6400",
			wantA: "256", wantB: "6400"},
		{name: "identifier_drift", a: "idleTimeout    = 20", b: "readTimeout    = 30",
			wantA: "idleTimeout    = 20", wantB: "readTimeout    = 30"},
		{name: "equal_strings", a: "x = 1", b: "x = 1", wantNone: true},
		{name: "multibyte_boundary", a: "a “b", b: "a ”b", wantA: "“b", wantB: "”b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotA, gotB, ok := driftRun(tc.a, tc.b)
			if tc.wantNone {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, tc.wantA, gotA)
			assert.Equal(t, tc.wantB, gotB)
			assert.True(t, utf8.ValidString(gotA))
			assert.True(t, utf8.ValidString(gotB))
		})
	}
}

func TestDriftNote(t *testing.T) {
	t.Parallel()

	t.Run("names_one_run", func(t *testing.T) {
		assert.Equal(t, `the file has "32000" where you wrote "6400"`,
			driftNote("window:  6400,", "window:  32000,"))
	})

	t.Run("names_every_drifted_line", func(t *testing.T) {
		got := driftNote("window:  6400,\nreserve: 256,", "window:  32000,\nreserve: 6400,")
		assert.Equal(t, `the file has "32000" where you wrote "6400", and "6400" where you wrote "256"`, got)
	})

	t.Run("caps_the_quotes", func(t *testing.T) {
		old := strings.Repeat("x = 1\n", maxDriftQuotes+2)
		got := driftNote(old, strings.Repeat("x = 2\n", maxDriftQuotes+2))
		assert.Equal(t, maxDriftQuotes, strings.Count(got, "where you wrote"))
	})

	t.Run("empty_when_identical", func(t *testing.T) {
		assert.Empty(t, driftNote("x = 1\ny = 2", "x = 1\ny = 2"))
	})
}

func TestTrailingPunct(t *testing.T) {
	t.Parallel()

	cases := []struct{ name, in, want string }{
		{"comma_after_value", "\t\treserve: 6400,", ","},
		{"none_after_ident", "\tidleTimeout = 90 * time.Second", ""},
		{"closing_paren", "\tf(x)", ")"},
		{"punctuation_only_line", "\t\t},", "\t\t},"}, // no token to stop the walk
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, trailingPunct(tc.in))
		})
	}
}

func TestFuzzyRefusesPunctuationLoss(t *testing.T) {
	t.Parallel()

	const buf = "func defaults() limits {\n\treturn limits{\n\t\twindow:  32000,\n\t\treserve: 6400,\n\t\tburst:   256,\n\t}\n}\n"

	t.Run("refuses_eating_the_comma", func(t *testing.T) {
		o, err := applyEdits(editTarget{Path: "main.go"}, buf,
			[]editOp{{OldText: "\t\treserve: 6...", NewText: "\t\treserve: 1200"}})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no match for edit 1")
		assert.Empty(t, o.final) // nothing written, so the file keeps its comma
	})

	t.Run("still_heals_when_punctuation_survives", func(t *testing.T) {
		o, err := applyEdits(editTarget{Path: "main.go"}, buf,
			[]editOp{{OldText: "\t\treserve: 256,", NewText: "\t\treserve: 12800,"}})
		require.NoError(t, err)
		assert.Contains(t, string(o.final), "reserve: 12800,")
		assert.Contains(t, string(o.final), "burst:   256,")
	})
}
