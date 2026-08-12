package command

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// fakeConsole is a recording Console for built-in command tests. It captures
// notices and prints, records model/reasoning/tools/started changes, and lets a
// test drive Pick/MultiPick with canned results.
type fakeConsole struct {
	mu sync.Mutex

	notices []string
	prints  []string

	picks      []fakePick      // queued results, consumed in order
	multiPicks []fakeMultiPick // queued results, consumed in order

	models       *llm.Registry
	state        *agent.State
	tools        *tools.Registry
	commands     *Registry
	started      bool
	setModel     llm.Model
	reasoning    llm.ReasoningConfig
	toolsChanged int
	exited       bool
	compactCalls int
	compactInstr string
}

type fakePick struct {
	result int   // returned when err is nil
	err    error // returned as-is
}

type fakeMultiPick struct {
	result []int
	err    error
}

func newFakeConsole(tb testing.TB) *fakeConsole {
	tb.Helper()
	// a small registry with two models so /model can resolve and list
	file := llm.File{Providers: map[string]llm.ProviderConfig{
		"test": {
			APIKeyEnv: "TEST_API_KEY",
			Models: []llm.ModelConfig{
				{ID: "alpha", Name: "Alpha", Aliases: []string{"a"}, ContextWindow: ptrInt(200000)},
				{ID: "beta", Name: "Beta", ContextWindow: ptrInt(128000)},
			},
		},
	}}
	reg, _ := llm.NewRegistry(file, nil, llm.RegistryOptions{Env: func(string) string { return "" }})

	tr := tools.New()
	return &fakeConsole{
		models:   reg,
		state:    &agent.State{Model: reg.Active(), Reasoning: llm.ReasoningConfig{Level: llm.LevelMedium, Show: true}},
		tools:    tr,
		commands: NewRegistry(),
	}
}

func ptrInt(v int) *int { return &v }

func (f *fakeConsole) Notify(msg string, level tui.Level) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notices = append(f.notices, msg)
	_ = level
}

func (f *fakeConsole) Print(markdown string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prints = append(f.prints, markdown)
}

func (f *fakeConsole) Pick(_ context.Context, prompt string, _ []tui.PickItem, _ tui.PickOptions) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.picks) == 0 {
		return 0, tui.ErrCancelled
	}
	next := f.picks[0]
	f.picks = f.picks[1:]
	return next.result, next.err
}

func (f *fakeConsole) MultiPick(_ context.Context, prompt string, _ []tui.PickItem, _ tui.MultiPickOptions) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.multiPicks) == 0 {
		return nil, tui.ErrCancelled
	}
	next := f.multiPicks[0]
	f.multiPicks = f.multiPicks[1:]
	return next.result, next.err
}

func (f *fakeConsole) Models() *llm.Registry  { return f.models }
func (f *fakeConsole) State() *agent.State    { return f.state }
func (f *fakeConsole) Tools() *tools.Registry { return f.tools }
func (f *fakeConsole) Commands() *Registry    { return f.commands }
func (f *fakeConsole) SetModel(m llm.Model)   { f.models.SetActive(m); f.state.Model = m; f.setModel = m }
func (f *fakeConsole) SetReasoning(c llm.ReasoningConfig) {
	f.reasoning = c
	f.state.Reasoning = c
}
func (f *fakeConsole) ToolsChanged() { f.mu.Lock(); f.toolsChanged++; f.mu.Unlock() }
func (f *fakeConsole) Started() bool { return f.started }
func (f *fakeConsole) Exit()         { f.exited = true }

func (f *fakeConsole) Compact(_ context.Context, instructions string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compactCalls++
	f.compactInstr = instructions
	return nil
}

// noticesSeen returns a snapshot of recorded notices.
func (f *fakeConsole) noticesSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.notices...)
}

// noticeContains reports whether any recorded notice contains substr.
func (f *fakeConsole) noticeContains(substr string) bool {
	for _, n := range f.noticesSeen() {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}
