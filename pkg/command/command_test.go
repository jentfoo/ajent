package command

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistryRegisterGetList(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(Command{Name: "help", Description: "h"})
	r.Register(Command{Name: "model", Description: "m"})

	got, ok := r.Get("model")
	require.True(t, ok)
	assert.Equal(t, "model", got.Name)

	_, ok = r.Get("ghost")
	assert.False(t, ok)

	names := r.Names()
	assert.Equal(t, []string{"help", "model"}, names)

	list := r.List()
	require.Len(t, list, 2)
	assert.Equal(t, "help", list[0].Name)
}

func TestRegistryReplaceKeepsOrder(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register(Command{Name: "help", Description: "v1"})
	r.Register(Command{Name: "model"})
	r.Register(Command{Name: "help", Description: "v2"}) // replace, no new entry

	require.Len(t, r.List(), 2)
	help, _ := r.Get("help")
	assert.Equal(t, "v2", help.Description)
	assert.Equal(t, []string{"help", "model"}, r.Names(), "order preserved on replace")
}

func TestRegistryHandlerInvoked(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	var called bool
	r.Register(Command{
		Name:    "ping",
		Handler: func(_ context.Context, _ string, _ Console) error { called = true; return nil },
	})

	cmd, ok := r.Get("ping")
	require.True(t, ok)
	require.NoError(t, cmd.Handler(context.Background(), "", nil))
	assert.True(t, called)
}
