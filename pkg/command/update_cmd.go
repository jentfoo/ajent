package command

import (
	"context"

	"github.com/jentfoo/ajent/pkg/tui"
	"github.com/jentfoo/ajent/pkg/version"
)

// updateCommand runs a self-update in the background and reports the outcome as
// notices. It returns immediately so it never blocks the prompt pump; Notify is
// safe from any goroutine.
func updateCommand(_ context.Context, _ string, c Console) error {
	c.Notify("checking for updates...", tui.LevelInfo)
	go func() {
		res := version.SelfUpdate(context.Background())
		if res.Err != nil {
			c.Notify(res.Notice(), levelError)
		} else {
			c.Notify(res.Notice(), levelInfo)
		}
	}()
	return nil
}
