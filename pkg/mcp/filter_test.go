package mcp

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
)

func defs(names ...string) []ToolDef {
	out := make([]ToolDef, len(names))
	for i, n := range names {
		out[i] = ToolDef{Name: n}
	}
	return out
}

// TestFilterToolsExclude verifies exact-name exclusions drop tools regardless of
// allow/deny globs.
func TestFilterToolsExclude(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		in      []ToolDef
		filter  ToolFilter
		exclude []string
		want    []string
	}{
		{
			name:    "drops exact names",
			in:      defs("a", "b", "c"),
			exclude: []string{"b"},
			want:    []string{"a", "c"},
		},
		{
			name:    "no exclusion admits all",
			in:      defs("a", "b"),
			exclude: nil,
			want:    []string{"a", "b"},
		},
		{
			name:    "combines with deny globs",
			in:      defs("read_x", "write_y", "secret_z"),
			filter:  ToolFilter{Deny: []string{"*_z"}},
			exclude: []string{"write_y"},
			want:    []string{"read_x"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := filterTools(slices.Clone(tc.in), tc.filter, tc.exclude)
			assert.Equal(t, tc.want, namesOf(got))
		})
	}
}

func namesOf(defs []ToolDef) []string {
	out := make([]string, len(defs))
	for i, d := range defs {
		out[i] = d.Name
	}
	return out
}
