package tools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// decoyFile holds two near-identical functions. Line scoring picks the earlier
// one (its lines share more tokens); only whole-block scoring reaches the later,
// correct region.
const decoyFile = "package main\n\nfunc oldGreet(name string) string {\n" +
	"\tif name == \"\" {\n\t\treturn \"hi\"\n\t}\n\treturn name\n}\n\n" +
	"func greet(name string) string {\n" +
	"\tif name == \"\" {\n\t\treturn \"hello, world\"\n\t}\n\treturn name\n}\n"

func TestClosestBlock(t *testing.T) {
	t.Parallel()

	t.Run("rejects_a_decoy_region", func(t *testing.T) {
		text, line, ok := closestBlock(
			"    if name == \"\" {\n        return \"hello, wrld\"\n    }", decoyFile)
		require.True(t, ok)
		assert.Equal(t, 11, line) // the real target, not the near-identical block at line 4
		assert.Equal(t, "\tif name == \"\" {\n\t\treturn \"hello, world\"\n\t}", text)
	})

	t.Run("preserves_indentation_verbatim", func(t *testing.T) {
		text, _, ok := closestBlock("if name == \"\" {\n    return \"hello, world\"\n}", decoyFile)
		require.True(t, ok)
		assert.True(t, strings.HasPrefix(text, "\tif"))    // real tabs, not trimmed
		assert.Contains(t, text, "\n\t\treturn \"hello, ") // and the nested depth
	})

	t.Run("finds_inline_fragment", func(t *testing.T) {
		// no whole-line anchor exists, so the token shortlist has to carry it
		text, line, ok := closestBlock("MaxLineRunes = 1050",
			"package tools\n\nconst MaxLineRunes = 1024\n")
		require.True(t, ok)
		assert.Equal(t, 3, line)
		assert.Equal(t, "const MaxLineRunes = 1024", text)
	})

	t.Run("silent_when_nothing_close", func(t *testing.T) {
		_, _, ok := closestBlock("completely unrelated content here",
			"package main\n\nfunc f() {}\n")
		assert.False(t, ok) // a decoy would be worse than admitting nothing matches
	})

	t.Run("clamps_to_file_length", func(t *testing.T) {
		text, line, ok := closestBlock("one\ntwo\nthree\nfour", "one\ntwo\n")
		require.True(t, ok)
		assert.Equal(t, 1, line)
		assert.Equal(t, "one\ntwo", text) // clamped to what the file holds
	})

	t.Run("empty_inputs", func(t *testing.T) {
		_, _, ok := closestBlock("   ", "anything\n")
		assert.False(t, ok)
		_, _, ok = closestBlock("x", "")
		assert.False(t, ok)
	})

	// a line repeated throughout the file must not anchor every candidate
	t.Run("ignores_a_ubiquitous_line", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 200; i++ {
			b.WriteString("}\n")
		}
		b.WriteString("func target() {\n\tcall()\n}\n")
		text, _, ok := closestBlock("func target() {\n\tcall()\n}", b.String())
		require.True(t, ok)
		assert.Equal(t, "func target() {\n\tcall()\n}", text)
	})
}

func TestBlockSimilarity(t *testing.T) {
	t.Parallel()

	assert.InDelta(t, 1.0, blockSimilarity("same", "same"), 0.001)
	assert.InDelta(t, 0.0, blockSimilarity("", "x"), 0.001)
	assert.Greater(t, blockSimilarity("hello world", "hello worlds"), 0.9)
	assert.Less(t, blockSimilarity("hello world", "totally other"), 0.5)
}

func TestMissingErrorHint(t *testing.T) {
	t.Parallel()

	t.Run("ends_with_copyable_text", func(t *testing.T) {
		msg := missingError(1, editTarget{Path: "greet.go"},
			"    if name == \"\" {\n        return \"hello, wrld\"\n    }", decoyFile, nil)
		assert.Contains(t, msg, "closest text in greet.go at line 11 (must match verbatim):")
		// the block is last and untouched, so it can be copied straight out
		assert.True(t, strings.HasSuffix(msg, "\tif name == \"\" {\n\t\treturn \"hello, world\"\n\t}"))
	})

	t.Run("says_no_similar_text", func(t *testing.T) {
		msg := missingError(1, editTarget{Path: "a.go"}, "unrelated content", "func f() {}\n", nil)
		assert.Contains(t, msg, "no similar text found in a.go")
		assert.NotContains(t, msg, "must match verbatim")
	})
}

func TestSelfReplaceIssue(t *testing.T) {
	t.Parallel()

	ops := []editOp{{OldText: "a", NewText: "b"}, {OldText: "same", NewText: "same"}}

	assert.Empty(t, selfReplaceIssue(1, ops))
	assert.Contains(t, selfReplaceIssue(2, ops), "nothing changes")
	assert.Empty(t, selfReplaceIssue(1, nil)) // a short op list must not panic
	assert.Empty(t, selfReplaceIssue(0, ops))
}

func TestCascadeIssue(t *testing.T) {
	t.Parallel()

	ops := []editOp{{OldText: "foo", NewText: "bar"}, {OldText: "bar", NewText: "baz"}}

	t.Run("names_the_right_edits", func(t *testing.T) {
		msg := cascadeIssue(2, "bar", ops)
		require.NotEmpty(t, msg)
		assert.Contains(t, msg, "edit 1's newText would create it")
		assert.Contains(t, msg, "combine edits 1 and 2 into one edit") // not "1 and 3"
	})

	t.Run("growth_not_cascade", func(t *testing.T) {
		// growing text in place is the common shape of a valid edit
		grow := []editOp{{OldText: "foo", NewText: "foo bar"}}
		assert.Empty(t, cascadeIssue(1, "foo", grow))
	})

	t.Run("tolerates_a_short_op_list", func(t *testing.T) {
		assert.Empty(t, cascadeIssue(1, "x", nil))
	})
}

func TestWhitespaceIssue(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		oldLine, fileLine string
		want              []string // empty means the lines space their words alike
	}{
		{"tab_file_space_text", " foo()", "\tfoo()",
			[]string{"indentation differs", "the file line uses 1 tab", "your text has 1 space"}},
		{"depth_differs", "  foo()", "    foo()",
			[]string{"the file line uses 4 spaces", "your text has 2 spaces"}},
		{"file_flush_left", "\tfoo()", "foo()",
			[]string{"the file line uses no indentation", "your text has 1 tab"}},
		{"text_flush_left", "foo()", "\tfoo()",
			[]string{"the file line uses 1 tab", "your text has no indentation"}},
		{"mixed_run_named_exactly", "\t foo()", "\t\tfoo()",
			[]string{"the file line uses 2 tabs", "mixed 1 tab and 1 space"}},
		{"inter_word", "var  a = 1", "var a = 1",
			[]string{"spacing between words differs", "the file has 1 space", "your text has 2 spaces"}},
		{"indentation_reported_first", "  var  a = 1", "\tvar a = 1",
			[]string{"indentation differs"}},
		{"identical", "\tfoo()", "\tfoo()", nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := whitespaceIssue(tc.oldLine, tc.fileLine)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			for _, w := range tc.want {
				assert.Contains(t, got, w)
			}
		})
	}
}

func TestLineWhitespaceIssuesAreBounded(t *testing.T) {
	t.Parallel()

	t.Run("groups_one_repeated_difference", func(t *testing.T) {
		var old, buf strings.Builder
		for i := 0; i < 40; i++ {
			fmt.Fprintf(&old, "  line%d()\n", i)
			fmt.Fprintf(&buf, "\tline%d()\n", i)
		}
		issues := lineWhitespaceIssues(old.String(), buf.String())
		require.Len(t, issues, 1) // not one bullet per line
		assert.Contains(t, issues[0], "lines 1, 2, 3 and 37 more")
		assert.Contains(t, issues[0], "indentation differs")
	})

	t.Run("caps_distinct_differences", func(t *testing.T) {
		var old, buf strings.Builder
		for i := 0; i < 9; i++ {
			fmt.Fprintf(&old, "%sline%d()\n", strings.Repeat(" ", i+1), i)
			fmt.Fprintf(&buf, "\tline%d()\n", i)
		}
		issues := lineWhitespaceIssues(old.String(), buf.String())
		require.Len(t, issues, maxWhitespaceIssues+1)
		assert.Contains(t, issues[maxWhitespaceIssues], "and 4 more spacing differences")
	})
}

func TestSoleDifference(t *testing.T) {
	t.Parallel()

	t.Run("quotes_whole_tokens", func(t *testing.T) {
		// a shared edge digit would otherwise report "409" against "6553"
		assert.Equal(t, `you wrote "4096" where the file has "65536"; everything else matches`,
			soleDifference("payloadBytes = 4096", "payloadBytes = 65536"))
	})

	t.Run("empty_when_identical", func(t *testing.T) {
		assert.Empty(t, soleDifference("x = 1", "x = 1"))
	})

	t.Run("empty_when_multiline", func(t *testing.T) {
		assert.Empty(t, soleDifference("a\nb", "a\nc\nd"))
	})

	t.Run("empty_when_too_long", func(t *testing.T) {
		long := strings.Repeat("z", diffQuoteLimit+1)
		assert.Empty(t, soleDifference("x = "+long, "x = "+strings.Repeat("y", diffQuoteLimit+1)))
	})
}

func TestStripTrailingWS(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"trims_each_line", "a  \nb\t\nc", "a\nb\nc"},
		{"keeps_indent_and_interior", "\ta  b  \n", "\ta  b\n"},
		{"trims_unicode_space", "a  ", "a"},
		{"unchanged_without_trailing", "a\nb", "a\nb"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, stripTrailingWS(tc.in))
		})
	}
}
