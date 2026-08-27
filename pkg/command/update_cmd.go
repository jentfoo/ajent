package command

import (
	"context"

	"github.com/jentfoo/ajent/pkg/tui"
)

// updateCommand runs a self-update in the background and reports the outcome as
// notices. It returns immediately so it never blocks the prompt pump; Notify is
// safe from any goroutine.
func updateCommand(_ context.Context, _ string, c Console) error {
	c.Notify("checking for updates...", tui.LevelInfo)
	go func() {
		res := SelfUpdate(context.Background())
		if res.Err != nil {
			c.Notify(res.Notice(), levelError)
		} else {
			c.Notify(res.Notice(), levelInfo)
		}
	}()
	return nil
}

// Notice renders one self-update result as a human line for the UI.
func (r UpdateResult) Notice() string {
	switch {
	case r.Err != nil:
		return "update failed: " + r.Err.Error()
	case !r.Installed:
		return "ajent is already up to date (" + versionLabel(r.Current) + ")"
	default:
		return "updated ajent from " + versionLabel(r.Current) +
			" to " + versionLabel(r.Latest)
	}
}

// versionLabel keeps a bare "dev" readable while leaving real versions as-is.
func versionLabel(v string) string {
	if v == "" || v == "dev" {
		return "v0.0.0-dev"
	}
	return v
}
