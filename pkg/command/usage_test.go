package command

import (
	"context"
	"strings"
	"testing"

	"github.com/jentfoo/ajent/pkg/tui"

	"github.com/jentfoo/ajent/pkg/llm"
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
	a.Response("test/alpha", llm.Usage{Input: 1000, Output: 200}, 900)
	child := a.Child()
	child.Response("test/alpha", llm.Usage{Input: 300, Output: 50}, 250)

	err := usageCommand(context.Background(), "", c)
	require.NoError(t, err)

	out := strings.Join(c.prints, "\n")
	ct := a.ChildTotal() // the delegated subset is exactly what /usage shows
	assert.Contains(t, out, "of which sub-agents:")
	assert.Contains(t, out, tui.FormatTokens(ct.Input)+" in / "+tui.FormatTokens(ct.Output)+" out")
}

func TestUsageOmitsChildRowWithoutDelegation(t *testing.T) {
	t.Parallel()

	c := newFakeConsole(t)
	a := tokens.New(llm.Model{ID: "alpha", Provider: "test"})
	a.Response("test/alpha", llm.Usage{Input: 1000, Output: 200}, 900)
	c.state.Tokens = a

	err := usageCommand(context.Background(), "", c)
	require.NoError(t, err)

	out := strings.Join(c.prints, "\n")
	assert.NotContains(t, out, "of which sub-agents:")
}
