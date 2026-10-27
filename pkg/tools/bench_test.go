package tools

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// BenchmarkBound measures the three output shapes every tool result passes
// through: the early return when both bounds hold, a line-budget trim, and a
// byte budget consumed by one overlong line.
func BenchmarkBound(b *testing.B) {
	for _, tc := range []struct {
		name  string
		in    string
		limit Limit
		trims bool
	}{
		{"fits_without_trim", strings.Repeat("func step(n int) int { return n % 97 }\n", 400),
			Limit{Lines: 2000, Bytes: 1 << 20}, false},
		{"line_budget_trim", strings.Repeat("func step(n int) int { return n % 97 }\n", 2000),
			Limit{Lines: 200, Bytes: 1 << 20}, true},
		{"byte_budget_overlong_line", strings.Repeat("x", 256<<10),
			Limit{Lines: 2000, Bytes: 8 << 10}, true},
	} {
		b.Run(tc.name, func(b *testing.B) {
			got := Bound(tc.in, tc.limit)
			require.Equal(b, tc.trims, got.Truncated)
			require.NotEmpty(b, got.Text)

			for b.Loop() {
				Bound(tc.in, tc.limit)
			}
		})
	}
}

// benchFileLines sizes the edit fixture: ~500 handlers at six lines each.
const benchFileLines = 3000

// benchFileContent is the large Go-like file the edit benchmarks run against.
func benchFileContent() string {
	var sb strings.Builder
	for i := 0; i < benchFileLines; i += 6 {
		fmt.Fprintf(&sb, "func handler_%04d(ctx context.Context, n int) error {\n", i)
		sb.WriteString("\tif n <= 0 {\n\t\treturn fmt.Errorf(\"bad input %d\", n)\n\t}\n\treturn nil\n}\n\n")
	}
	return sb.String()
}

// benchEditArgs builds a multi-edit call body touching six sites across the
// file, flipping each site between its two states so the ops alternate
// involution-style and every iteration starts from the previous one's result.
func benchEditArgs(b *testing.B, flip bool) string {
	b.Helper()

	from, to := "if n <= 0 {", "if n < 0 {"
	if flip {
		from, to = to, from
	}
	ops := make([]editOp, 0, 6)
	for i := range 6 {
		n := i * 480
		ops = append(ops, editOp{
			OldText: fmt.Sprintf("func handler_%04d(ctx context.Context, n int) error {\n\t%s", n, from),
			NewText: fmt.Sprintf("func handler_%04d(ctx context.Context, n int) error {\n\t%s", n, to),
		})
	}
	raw, err := json.Marshal(editParams{Path: "big.go", Edits: ops})
	require.NoError(b, err)
	return string(raw)
}

// BenchmarkEditExecute drives the edit tool the way the registry does: read the
// few-thousand-line file, resolve every op, write once. The apply case flips
// six sites back and forth, so no file restore is needed; the missing-match
// case mutates nothing and runs the diagnostic path instead.
func BenchmarkEditExecute(b *testing.B) {
	e := newToolEnv(b.TempDir())
	e.writeFile("big.go", benchFileContent())
	apply := []string{benchEditArgs(b, false), benchEditArgs(b, true)}
	missing := `{"path":"big.go","edits":[{"oldText":"func handler_9999(ctx context.Context, n int) error {\n\tif n <= 0 {","newText":"x"}]}`
	b.ReportMetric(float64(benchFileLines), "file-lines")

	res := e.editExec(b.Context(), apply[0])
	require.False(b, res.IsError, textOf(res))
	res = e.editExec(b.Context(), missing)
	require.True(b, res.IsError, textOf(res))
	require.Contains(b, textOf(res), "no match")

	b.Run("multi_edit_apply", func(b *testing.B) {
		for i := 1; b.Loop(); i++ { // warm-up applied apply[0], so flip back first
			if res := e.editExec(b.Context(), apply[i%2]); res.IsError {
				b.Fatal(textOf(res))
			}
		}
	})
	b.Run("missing_match_diag", func(b *testing.B) {
		for b.Loop() {
			if res := e.editExec(b.Context(), missing); !res.IsError {
				b.Fatal(textOf(res))
			}
		}
	})
}

// BenchmarkReadExecute drives the read tool the way the registry does: sniff and
// full file read, line numbering and tracker observation, first over the default
// window and then over an explicit page deep in the file.
func BenchmarkReadExecute(b *testing.B) {
	e := newToolEnv(b.TempDir())
	e.writeFile("big.go", benchFileContent())
	b.ReportMetric(float64(benchFileLines), "file-lines")

	defaults := `{"path":"big.go"}`
	page := fmt.Sprintf(`{"path":"big.go","offset":%d,"limit":200}`, benchFileLines-400)
	for _, tc := range []struct{ name, args string }{
		{"default_window", defaults},
		{"explicit_page", page},
	} {
		b.Run(tc.name, func(b *testing.B) {
			res := e.readExec(b.Context(), tc.args)
			require.False(b, res.IsError, textOf(res))

			for b.Loop() {
				e.readExec(b.Context(), tc.args)
			}
		})
	}
}

// benchGrepTree is ~200KB of synthetic Go source with a spread of matchable
// lines, what a workspace-wide fallback search walks when rg is absent.
func benchGrepTree() string {
	var sb strings.Builder
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&sb, "func handler_%04d(ctx context.Context, n int) error {\n", i)
		sb.WriteString("\tif n <= 0 {\n\t\treturn fmt.Errorf(\"bad input %d\", n)\n\t}\n\treturn nil\n}\n\n")
	}
	return sb.String()
}

// BenchmarkGrepFallback measures the pure Go fallback engine over one large
// file: a rare literal match, a regex, dense matches and context windows.
func BenchmarkGrepFallback(b *testing.B) {
	content := benchGrepTree()
	b.ReportMetric(float64(len(content)), "src-B/op")

	for _, tc := range []struct {
		name    string
		pattern string
		ctx     int
	}{
		{"rare_match", `handler_2999`, 0},
		{"regex_match", `handler_0\d\d[1-5]`, 0},
		{"many_matches", `return`, 0},
		{"with_context", `handler_15`, 3},
	} {
		b.Run(tc.name, func(b *testing.B) {
			re := regexp.MustCompile(tc.pattern)
			hits, n, _ := goGrep(content, re, tc.ctx, 0)
			require.Positive(b, n)
			require.NotEmpty(b, hits)

			for b.Loop() {
				goGrep(content, re, tc.ctx, 0)
			}
		})
	}
}
