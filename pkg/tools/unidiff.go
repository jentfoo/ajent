package tools

import (
	"strings"

	udiff "github.com/aymanbagabas/go-udiff"
)

// diffContext is how many lines of unchanged text frame each hunk.
const diffContext = 3

// unifiedDiff renders a plain unified diff of before and after, empty when
// they match. fromName and toName label the ---/+++ headers; a single-path
// change passes the same name twice. pkg/tools never imports pkg/tui, so this
// drives go-udiff directly rather than reusing the theme-coupled renderer
// there. The edit tool and the git_* readers share it.
func unifiedDiff(fromName, toName, before, after string) string {
	if before == after {
		return ""
	}
	edits := udiff.Lines(before, after)
	if len(edits) == 0 {
		return ""
	}
	out, err := udiff.ToUnified(fromName, toName, before, edits, diffContext)
	if err != nil || out == "" {
		return ""
	}
	return strings.TrimRight(out, "\n")
}
