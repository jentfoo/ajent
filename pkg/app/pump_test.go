package app

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/tui"
)

func TestRunPump(t *testing.T) {
	t.Parallel()

	t.Run("serves_then_exits_on_cancel", func(t *testing.T) {
		inR, inW, err := os.Pipe()
		require.NoError(t, err)
		outR, outW, err := os.Pipe()
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = inR.Close()
			_ = inW.Close()
			_ = outR.Close()
			_ = outW.Close()
		})
		go func() { _, _ = io.Copy(io.Discard, outR) }() // status writes must not fill the pipe
		ui, err := tui.New(tui.Options{In: inR, Out: outW, Mode: tui.ModePlain})
		require.NoError(t, err)
		t.Cleanup(ui.Close)

		cmds := command.NewRegistry()
		handled := make(chan string, 4)
		cmds.Register(command.Command{
			Name: "ping", Description: "test",
			Handler: func(_ context.Context, arg string, _ command.Console) error {
				handled <- arg
				return nil
			},
		})
		console := &uiConsole{ui: ui, commands: cmds}
		pump := make(chan pumpLine, 4)
		var started bool
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			runPump(ctx, pump, nil, console, command.NewStager(ctx, nil, nil), nil,
				false, ui, &started, func() {},
				newSteerQueue(&fakeQueueUI{}, func(int) {}, func() {}),
				nil, &agent.State{}, nil, &sync.Once{}, func() {}, planHooks{})
			close(done)
		}()

		pump <- pumpLine{kind: command.KindCommand, rest: "ping hi"}
		select {
		case got := <-handled:
			assert.Equal(t, "hi", got)
		case <-time.After(5 * time.Second):
			t.Fatal("command never ran")
		}

		cancel() // the driver's quit path: no close(pump), just the root context
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("runPump did not exit on cancel")
		}
		// the channel outlives runPump, so a late sender can never hit a close
		require.NotPanics(t, func() { pump <- pumpLine{kind: command.KindCommand, rest: "ping late"} })
	})
}
