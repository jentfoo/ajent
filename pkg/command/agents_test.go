package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAgents is a canned Agents backing /agents tests.
type fakeAgents struct {
	jobs     []AgentJob
	stopped  []string // ids Stop was asked to cancel, in order
	stopsAll int      // StopAll call count
}

func (f *fakeAgents) List() []AgentJob { return f.jobs }
func (f *fakeAgents) Stop(id string) error {
	f.stopped = append(f.stopped, id)
	for _, j := range f.jobs {
		if j.ID == id && j.Status != "done" && j.Status != "error" {
			return nil
		}
	}
	return errAlreadyFinished
}

var errAlreadyFinished = &fakeErr{"already finished"}

type fakeErr struct{ msg string }

func (e *fakeErr) Error() string { return e.msg }

func (f *fakeAgents) StopAll() int {
	f.stopsAll++
	n := 0
	for _, j := range f.jobs {
		if j.Status == "running" || j.Status == "queued" {
			n++
		}
	}
	return n
}

// agentsConsole returns a fake console with canned sub-agent rows.
func agentsConsole(tb testing.TB) (*fakeConsole, *fakeAgents) {
	tb.Helper()
	c := newFakeConsole(tb)
	a := &fakeAgents{jobs: []AgentJob{
		{ID: "sub-1", Status: "running", Task: "grep func New", Elapsed: 41 * time.Second},
		{ID: "sub-2", Status: "done", Task: "ls pkg/agent", Elapsed: 12 * time.Second},
	}}
	c.agents = a
	return c, a
}

func TestAgentsCommand(t *testing.T) {
	t.Parallel()

	// the list verb renders a table.
	t.Run("list_renders_table", func(t *testing.T) {
		c, _ := agentsConsole(t)
		err := agentsCommand(context.Background(), "", c)
		require.NoError(t, err)

		out := strings.Join(c.prints, "\n")
		assert.Contains(t, out, "# Sub-agents")
		assert.Contains(t, out, "| sub-1 | running | 41s | grep func New |")
		assert.Contains(t, out, "| sub-2 | done | 12s | ls pkg/agent |")
	})

	t.Run("stop_one_cancels", func(t *testing.T) {
		c, a := agentsConsole(t)
		err := agentsCommand(context.Background(), "stop sub-1", c)
		require.NoError(t, err)
		assert.Equal(t, []string{"sub-1"}, a.stopped)
		assert.True(t, c.noticeContains("stopping"))
	})

	t.Run("stop_finished_warns", func(t *testing.T) {
		c, _ := agentsConsole(t)
		err := agentsCommand(context.Background(), "stop sub-2", c)
		require.NoError(t, err)
		assert.True(t, c.noticeContains("already finished"))
	})

	t.Run("stop_all_cancels_every_job", func(t *testing.T) {
		c, a := agentsConsole(t)
		err := agentsCommand(context.Background(), "stop all", c)
		require.NoError(t, err)
		assert.Equal(t, 1, a.stopsAll) // only in-flight (running/queued) jobs cancel
		assert.True(t, c.noticeContains("stopped 1 sub-agent(s)"))
	})

	t.Run("unknown_verb_warns", func(t *testing.T) {
		c, _ := agentsConsole(t)
		err := agentsCommand(context.Background(), "bogus", c)
		require.NoError(t, err)
		assert.True(t, c.noticeContains(`unknown /agents verb "bogus"`))
	})

	t.Run("unavailable_notifies", func(t *testing.T) {
		c := newFakeConsole(t) // agents nil
		err := agentsCommand(context.Background(), "", c)
		require.NoError(t, err)
		assert.True(t, c.noticeContains("sub-agents not available"))
	})
}

func TestAgentsCompletion(t *testing.T) {
	t.Parallel()

	// verbs then ids are offered.
	t.Run("offers_verbs_then_ids", func(t *testing.T) {
		c, _ := agentsConsole(t)
		fn := agentsCompletion(c)

		verbs := fn("")
		require.Contains(t, verbs, "list")
		require.Contains(t, verbs, "stop")

		ids := fn("stop ")
		assert.Equal(t, []string{"all", "sub-1", "sub-2"}, ids)

		picked := fn("stop sub-")
		assert.Equal(t, []string{"sub-1", "sub-2"}, picked)
	})

	t.Run("unavailable_nil", func(t *testing.T) {
		c := newFakeConsole(t)
		fn := agentsCompletion(c)
		assert.Nil(t, fn(""))
	})
}
