package plan

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/stretchr/testify/require"
)

var (
	plannerModel     = llm.Model{Provider: "p", ID: "planner"}
	implementorModel = llm.Model{Provider: "p", ID: "implementor"}
)

// forkCall is one recorded branch switch.
type forkCall struct {
	head  string
	model llm.Model
}

// fakeHost records everything the controller drives so tests can assert on the
// sequence rather than on a real session, registry or UI.
type fakeHost struct {
	mu sync.Mutex

	pick     llm.Model
	pickOK   bool
	running  bool
	head     string
	askIndex int
	askErr   error
	lastText string
	forkErr  error

	forks     []forkCall
	toolSets  [][]string
	added     []agent.Tool
	dropped   int
	persisted []persisted
	inputs    []string
	notices   []string
	statuses  []string
	aborted   int
	stored    *persisted
}

// newFakeHost returns a host wired for a planner pick, with the trunk head set.
func newFakeHost() *fakeHost {
	return &fakeHost{pick: plannerModel, pickOK: true, head: "trunk-tip"}
}

func (f *fakeHost) host() Host {
	return Host{
		PickModel: func(context.Context, string) (llm.Model, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.pick, f.pickOK
		},
		ActiveModel: func() llm.Model { return implementorModel },
		Running: func() bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.running
		},
		Abort: func() { f.mu.Lock(); f.aborted++; f.mu.Unlock() },

		ToolNames:    func() []string { return []string{"read", "write", "edit", "bash"} },
		PlannerTools: func() []string { return []string{"agent_start"} },
		SetTools: func(names []string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.toolSets = append(f.toolSets, slices.Clone(names))
		},
		AddTools: func(ts []agent.Tool) { f.mu.Lock(); f.added = ts; f.mu.Unlock() },
		DropTools: func() {
			f.mu.Lock()
			f.dropped++
			f.added = nil
			f.mu.Unlock()
		},

		Fork: func(head string, m llm.Model) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.forks = append(f.forks, forkCall{head: head, model: m})
			if f.forkErr != nil {
				return f.forkErr
			}
			f.head = head + "-tip"
			return nil
		},
		Head: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.head
		},

		Persist: func(v any) error {
			f.mu.Lock()
			defer f.mu.Unlock()
			p, _ := v.(persisted)
			f.persisted = append(f.persisted, p)
			return nil
		},
		Restore: func(v any) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.stored == nil {
				return false
			}
			raw, _ := json.Marshal(*f.stored)
			return json.Unmarshal(raw, v) == nil
		},
		ResolveModel: func(key string) (llm.Model, bool) {
			switch key {
			case plannerModel.Key():
				return plannerModel, true
			case implementorModel.Key():
				return implementorModel, true
			}
			return llm.Model{}, false
		},

		LastText: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.lastText
		},

		SetInput: func(s string) { f.mu.Lock(); f.inputs = append(f.inputs, s); f.mu.Unlock() },
		Ask: func(context.Context, string, []string) (int, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.askIndex, f.askErr
		},
		Notify: func(msg string, _ agent.Level) {
			f.mu.Lock()
			f.notices = append(f.notices, msg)
			f.mu.Unlock()
		},
		Status: func(text, _ string) { f.mu.Lock(); f.statuses = append(f.statuses, text); f.mu.Unlock() },
		Git: func(context.Context) (string, string) {
			return " M main.go", " main.go | 3 +-"
		},
	}
}

// lastTools returns the most recently applied tool set.
func (f *fakeHost) lastTools() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.toolSets) == 0 {
		return nil
	}
	return f.toolSets[len(f.toolSets)-1]
}

// started builds a controller already in the planning phase.
func started(t *testing.T) (*Controller, *fakeHost) {
	t.Helper()
	f := newFakeHost()
	c := New(f.host())
	require.Empty(t, c.Start(t.Context(), ""))
	return c, f
}

// call runs a control tool by name with the given JSON arguments.
func call(t *testing.T, c *Controller, name, args string) agent.ToolResult {
	t.Helper()
	for _, tool := range controlTools(c) {
		if tool.Name() != name {
			continue
		}
		res, err := tool.Execute(t.Context(), agent.ToolCall{
			Name: name, Input: json.RawMessage(args)}, nil)
		require.NoError(t, err) // control tools never fail a turn
		return res
	}
	t.Fatalf("no control tool named %s", name)
	return agent.ToolResult{}
}

// done is a cleanly finished turn.
func done() agent.TurnResult { return agent.TurnResult{Stop: llm.StopEndTurn} }

// agentCall builds a bare call for label checks.
func agentCall(name string) agent.ToolCall { return agent.ToolCall{Name: name} }
