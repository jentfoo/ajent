package command

import (
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/strutil"
	"github.com/jentfoo/ajent/pkg/tokens"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageShowsChildSpendWhenDelegated(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	a := tokens.New(llm.Model{ID: "alpha", Provider: "test"})
	c.state.Tokens = a

	// parent spends, then a child rolls its own spend up separately.
	a.Response("test/alpha", llm.Usage{Input: 1000, Output: 200}, 900, true)
	child := a.Child()
	child.Response("test/alpha", llm.Usage{Input: 300, Output: 50}, 250, true)

	err := usageCommand(t.Context(), "", c)
	require.NoError(t, err)

	out := strings.Join(c.prints, "\n")
	ct := a.ChildTotal() // the delegated subset is exactly what /usage shows
	assert.Contains(t, out, "of which sub-agents:")
	assert.Contains(t, out, strutil.FormatTokens(ct.Input)+" in / "+strutil.FormatTokens(ct.Output)+" out")
}

func TestUsageOmitsChildRowWithoutDelegation(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	a := tokens.New(llm.Model{ID: "alpha", Provider: "test"})
	a.Response("test/alpha", llm.Usage{Input: 1000, Output: 200}, 900, true)
	c.state.Tokens = a

	err := usageCommand(t.Context(), "", c)
	require.NoError(t, err)

	out := strings.Join(c.prints, "\n")
	assert.NotContains(t, out, "of which sub-agents:")
}

func TestUsagePrintsSessionLedger(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	// give the fake state a ledger with one reported turn so /usage has data.
	st := &agent.State{Model: llm.Model{ID: "alpha", Provider: "test"},
		Reasoning: llm.ReasoningConfig{}}
	tok := tokens.New(st.Model)
	const key = "test/alpha"
	tok.Response(key, llm.Usage{Input: 1000, Output: 200}, 900, true)
	st.Tokens = tok
	c.state = st

	cmd, ok := r.Get("usage")
	require.True(t, ok)
	require.NoError(t, cmd.Handler(t.Context(), "", c))

	require.Len(t, c.prints, 1)
	assert.Contains(t, c.prints[0], "# Usage")
	assert.Contains(t, c.prints[0], "input")
	assert.Contains(t, c.prints[0], "output")
}

func TestUsageWithNoLedgerNotifies(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	r := NewRegistry()
	c.commands = r
	RegisterBuiltins(r, c)

	c.state.Tokens = nil // no accounting configured

	cmd, ok := r.Get("usage")
	require.True(t, ok)
	require.NoError(t, cmd.Handler(t.Context(), "", c))

	assert.True(t, c.noticeContains("no accounting available"))
}
