package app

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/session"
)

// DeleteOptions names what --delete and --delete-old act on: one target by
// name or id, or the stale-session sweep over OldDays.
type DeleteOptions struct {
	Target  string // --delete <name|id>; empty runs the sweep instead
	OldDays int    // --delete-old window in days
}

// RunDelete removes saved sessions per --delete or --delete-old and returns
// the process exit code. Progress goes to out; in answers the sweep's
// confirmation.
func RunDelete(out io.Writer, in io.Reader, o DeleteOptions) int {
	store, err := session.NewStore()
	if err == nil {
		cwd := config.Cwd()
		if o.Target != "" {
			err = session.DeleteSession(out, store, cwd, o.Target)
		} else {
			cutoff := time.Now().UTC().AddDate(0, 0, -o.OldDays)
			err = session.DeleteOldSessions(out, in, store, cwd, cutoff)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		return ExitUsage
	}
	return ExitOK
}
