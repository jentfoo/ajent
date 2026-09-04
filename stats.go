package main

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// statsSink counts tool calls and failures by name. It observes the same events
// the drain does, so nothing in the agent loop has to know it is watching.
type statsSink struct {
	agent.NopSink

	mu     sync.Mutex
	calls  map[string]int
	failed map[string]int
}

func newStatsSink() *statsSink {
	return &statsSink{calls: make(map[string]int), failed: make(map[string]int)}
}

// ToolStart records the call, returning the hook that records how it ended.
func (s *statsSink) ToolStart(call agent.ToolCall, _ string) func(agent.ToolResult) {
	s.mu.Lock()
	s.calls[call.Name]++
	s.mu.Unlock()

	return func(res agent.ToolResult) {
		if !res.IsError {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.failed[call.Name]++
	}
}

// sessionStats is one run's totals, rendered as prose or as a json line.
type sessionStats struct {
	Turns   int                  `json:"turns"`
	Seconds float64              `json:"seconds"`
	Calls   map[string]int       `json:"calls,omitempty"`
	Failed  map[string]int       `json:"failed,omitempty"`
	Usage   llm.Usage            `json:"usage"`
	ByModel map[string]llm.Usage `json:"byModel,omitempty"`
}

// collect snapshots the run's totals. The token side is already tallied by the
// accounting ledger, so this only reads it.
func (s *statsSink) collect(acct *tokens.Accounting, elapsed time.Duration) sessionStats {
	s.mu.Lock()
	st := sessionStats{
		Seconds: elapsed.Round(time.Millisecond).Seconds(),
		Calls:   maps.Clone(s.calls),
		Failed:  maps.Clone(s.failed),
	}
	s.mu.Unlock()

	if acct != nil {
		st.Turns = acct.TurnsCount()
		st.Usage = acct.Total()
		st.ByModel = acct.ByModel()
	}
	return st
}

// writeStats renders the summary as prose, one prefixed line at a time, so a
// terminal reading stderr sees it beside the run's other notices.
func writeStats(w io.Writer, prefix string, s sessionStats) {
	line := func(format string, args ...any) {
		_, _ = fmt.Fprintf(w, prefix+format+"\n", args...)
	}
	line("--- session summary ---")
	line("turns %d  wall %s", s.Turns, time.Duration(s.Seconds*float64(time.Second)).Round(time.Millisecond))
	if tools := toolLine(s); tools != "" {
		line("tools  %s", tools)
	}
	line("tokens in %s  out %s  cache-r %s  cache-w %s",
		thousands(s.Usage.Input), thousands(s.Usage.Output),
		thousands(s.Usage.CacheRead), thousands(s.Usage.CacheWrite))
	for _, key := range slices.Sorted(maps.Keys(s.ByModel)) {
		u := s.ByModel[key]
		line("  %s  in %s  out %s", key, thousands(u.Input), thousands(u.Output))
	}
}

// toolLine renders per-tool counts in name order, marking failures.
func toolLine(s sessionStats) string {
	parts := make([]string, 0, len(s.Calls))
	for _, name := range slices.Sorted(maps.Keys(s.Calls)) {
		part := fmt.Sprintf("%s %d", name, s.Calls[name])
		if n := s.Failed[name]; n > 0 {
			part += fmt.Sprintf(" (%d failed)", n)
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "  ")
}

// thousands renders n with separators, since token counts are read at a glance.
func thousands(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
