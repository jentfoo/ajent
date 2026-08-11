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
		{"email_not_a_ref", "email@example.com", nil},
		{"mid_word_not_ref", "a@b", nil},
		{"trailing_punct_excluded", "@file,", []Ref{{Path: "file", Start: 0, End: 5}}},
		{"absorbs_annotation", "@file (800 lines, 64kb)", []Ref{
			{Path: "file", Start: 0, End: 23, Note: " (800 lines, 64kb)"},
		}},
		{"prose_paren_not_absorbed", "@file (see above)", []Ref{
			{Path: "file", Start: 0, End: 5},
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

	// re-parsing already-annotated text yields the ref with the note absorbed,
	// never a second annotation
	annotated := "@file (800 lines, 64kb) and @file (800 lines, 64kb)"
	refs := Parse(annotated)
	require.Len(t, refs, 2)
	for _, r := range refs {
		assert.Equal(t, "file", r.Path)
		assert.Equal(t, " (800 lines, 64kb)", r.Note)
	}
}
