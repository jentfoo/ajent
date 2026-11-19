package refs

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want []Ref
	}{
		{"plain_no_refs", "hello world", nil},
		{"single_ref", "@main.go", []Ref{{Path: "main.go", Start: 0, End: 8}}},
		{"two_refs", "@a.go and @b.go", []Ref{
			{Path: "a.go", Start: 0, End: 5},
			{Path: "b.go", Start: 10, End: 15},
		}},
		{"dir_trailing_slash", "@dir/", []Ref{{Path: "dir/", Start: 0, End: 5}}},
		{"after_paren", "see (@x.go)", []Ref{{Path: "x.go", Start: 5, End: 10}}},
		{"after_bracket", "[@y.go]", []Ref{{Path: "y.go", Start: 1, End: 6}}},
		{"bracketed_name_keeps_close", "@weird[1]", []Ref{
			{Path: "weird[1]", Start: 0, End: 9},
		}},
		{"bracketed_file_ref", "see @a[1].txt now", []Ref{
			{Path: "a[1].txt", Start: 4, End: 13},
		}},
		{"close_without_open_ends_token", "@f.go] rest", []Ref{
			{Path: "f.go", Start: 0, End: 5},
		}},
		{"escaped_space_one_ref", "see @my\\ notes.txt now", []Ref{
			{Path: "my notes.txt", Start: 4, End: 18},
		}},
		{"escaped_comma", "@report\\,v2.pdf", []Ref{
			{Path: "report,v2.pdf", Start: 0, End: 15},
		}},
		{"escaped_backslash_literal", "@a\\\\b.go", []Ref{
			{Path: "a\\b.go", Start: 0, End: 8},
		}},
		{"escaped_backslash_then_bracket", "@a\\\\[b.txt", []Ref{
			{Path: "a\\[b.txt", Start: 0, End: 10},
		}},
		{"close_escaped_open_bare", "@a\\]b[c]d", []Ref{
			{Path: "a]b[c]d", Start: 0, End: 9},
		}},
		{"escaped_punct_then_real_end", "@a\\,b, more", []Ref{
			{Path: "a,b", Start: 0, End: 5},
		}},
		{"escaped_then_second_ref", "@a\\ x @b.go", []Ref{
			{Path: "a x", Start: 0, End: 5},
			{Path: "b.go", Start: 6, End: 11},
		}},
		{"escaped_annotation_absorbed", "@f\\ 1.go (2 lines)", []Ref{
			{Path: "f 1.go", Start: 0, End: 18, Note: " (2 lines)"},
		}},
		{"lone_trailing_backslash_kept", "@x\\", []Ref{
			{Path: "x\\", Start: 0, End: 3},
		}},
		{"escaped_utf8_bytes_survive", "@caf\u00e9\\ bar.md", []Ref{
			{Path: "caf\u00e9 bar.md", Start: 0, End: 14},
		}},
		{"email_not_a_ref", "email@example.com", nil},
		{"mid_word_not_ref", "a@b", nil},
		{"trailing_punct_excluded", "@file,", []Ref{{Path: "file", Start: 0, End: 5}}},
		{"absorbs_annotation", "@file (800 lines, 64kb)", []Ref{
			{Path: "file", Start: 0, End: 23, Note: " (800 lines, 64kb)"},
		}},
		{"absorbs_special_annotation", "@pipe (special)", []Ref{
			{Path: "pipe", Start: 0, End: 15, Note: " (special)"},
		}},
		{"prose_paren_not_absorbed", "@file (see above)", []Ref{
			{Path: "file", Start: 0, End: 5},
		}},
		{"crlf_between_refs", "@a.go\r\n@b.go", []Ref{
			{Path: "a.go", Start: 0, End: 5},
			{Path: "b.go", Start: 7, End: 12},
		}},
		{"crlf_after_interior_ref", "see @a.go\r\nnext line", []Ref{
			{Path: "a.go", Start: 4, End: 9},
		}},
		{"cr_before_ref_at_boundary", "prev\r@a.go", []Ref{
			{Path: "a.go", Start: 5, End: 10},
		}},
		{"crlf_after_annotation", "@file (800 lines, 64kb)\r\ntext", []Ref{
			{Path: "file", Start: 0, End: 23, Note: " (800 lines, 64kb)"},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Parse(c.in)
			require.Len(t, got, len(c.want), "len mismatch for %q", c.in)
			for i := range got {
				assert.Equal(t, c.want[i].Path, got[i].Path)
				assert.Equal(t, c.want[i].Start, got[i].Start)
				assert.Equal(t, c.want[i].End, got[i].End)
				assert.Equal(t, c.want[i].Note, got[i].Note)
			}
		})
	}
}

func TestParseIdempotentAnnotation(t *testing.T) {
	t.Parallel()

	// re-parsing already-annotated text yields the ref with the note absorbed, never a second annotation
	annotated := "@file (800 lines, 64kb) and @file (800 lines, 64kb)"
	refs := Parse(annotated)
	require.Len(t, refs, 2)
	for _, r := range refs {
		assert.Equal(t, "file", r.Path)
		assert.Equal(t, " (800 lines, 64kb)", r.Note)
	}
}

func TestEscapePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain_unchanged", "main.go", "main.go"},
		{"space", "my notes.txt", `my\ notes.txt`},
		{"comma", "report,v2.pdf", `report\,v2.pdf`},
		{"semicolon", "a;b", `a\;b`},
		{"bang", "a!b", `a\!b`},
		{"question", "a?b", `a\?b`},
		{"close_paren", "a)b", `a\)b`},
		{"close_brace", "a}b", `a\}b`},
		{"double_quote", `a"b`, `a\"b`},
		{"apostrophe", "a'b", `a\'b`},
		{"backtick", "a`b", "a\\`b"},
		{"backslash_doubled", `a\b`, `a\\b`},
		{"tab", "a\tb", "a\\\tb"},
		{"bracketed_pair_stays_bare", "weird[1]", "weird[1]"},
		{"unmatched_close_escaped", "a]b", `a\]b`},
		{"open_kept_bare", "[abc", "[abc"},
		{"dir_slash_untouched", "my notes/x.go", `my\ notes/x.go`},
		{"leading_break_escaped", " lead.txt", `\ lead.txt`},
		{"utf8_untouched", "caf\u00e9 bar", "caf\u00e9\\ bar"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, escapePath(c.in))
		})
	}

	// the invariant behind the completion round trip: an offered form parses
	// back to the exact name, however hostile
	t.Run("round_trips_through_parse", func(t *testing.T) {
		for _, c := range cases {
			refs := Parse("@" + escapePath(c.in))
			require.Len(t, refs, 1, c.in)
			assert.Equal(t, c.in, refs[0].Path, c.in)
		}
	})
}

func TestUnescapePath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain_unchanged", "main.go", "main.go"},
		{"removes_escapes", `my\ notes.txt`, "my notes.txt"},
		{"escaped_punct", `a\,b`, "a,b"},
		{"backslash_pair", `a\\b`, `a\b`},
		{"trailing_lone_backslash_kept", `x\`, `x\`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, unescapePath(c.in))
		})
	}

	// unescaping is exactly the inverse of escaping
	t.Run("inverts_escape", func(t *testing.T) {
		for _, name := range []string{
			"my notes.txt", "report,v2.pdf", "a\b", "weird[1]", "a]b", "[abc",
			"a;b!c?d)e}f'g\"h`i", "caf\u00e9 bar/two\twords", "x\\y\\z",
		} {
			assert.Equal(t, name, unescapePath(escapePath(name)), name)
		}
	})
}
