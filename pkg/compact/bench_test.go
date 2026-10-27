package compact

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/session"
)

// loadBenchBranch reads a committed transcript from the session package's
// corpus, the testing.T loader's *testing.B twin.
func loadBenchBranch(b *testing.B, name string) []session.Entry {
	b.Helper()

	entries, warns, err := session.Read(filepath.Join("..", "session", "testdata", "branches", name))
	require.NoError(b, err)
	require.Empty(b, warns)
	return session.Branch(entries, session.Head(entries))
}

// BenchmarkCompactPlan measures the structural stages that block the watched
// turn when compaction fires: branch decode and token estimation, cut choice
// and stub detection, with the summariser call stubbed out.
func BenchmarkCompactPlan(b *testing.B) {
	branch := loadBenchBranch(b, "tools.jsonl")
	model := llm.Model{Provider: "test", ID: "m", ContextWindow: 200000, MaxOutput: 4000}
	run := func(context.Context, llm.Request) (string, error) { return fullSummary, nil }
	opts := Options{Resolve: noResolve, Base: 4000, VerbatimTokens: 1} // force the band to its floor
	b.ReportMetric(float64(len(branch)), "entries/op")

	res, err := Compact(b.Context(), branch, model, run, opts)
	require.NoError(b, err)
	require.NotNil(b, res)

	b.ResetTimer()
	for b.Loop() {
		if _, err := Compact(b.Context(), branch, model, run, opts); err != nil {
			b.Fatal(err)
		}
	}
}
