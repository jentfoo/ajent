package app

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/client"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/session"
)

// DeleteOptions names what --delete and --delete-old act on: one target by
// name or id, or the stale-session sweep over OldDays. Force escapes the
// live-session refusal for a named --delete only; the sweep never forces.
type DeleteOptions struct {
	Target  string // --delete <name|id>, empty runs the sweep instead
	OldDays int    // --delete-old window in days
	Force   bool   // --force: delete a live session's transcript anyway
}

// servedSessions reports whether a session id has a live serving instance,
// from discovery records alone. A discovery failure reads as none served: the
// refusal is protection, not a gate nothing may bypass.
func servedSessions() func(id string) bool {
	insts, err := client.DiscoverLive()
	if err != nil {
		return nil
	}
	ids := bulk.SliceToSetBy(func(i client.Instance) string { return i.SessionID }, insts)
	return func(id string) bool {
		_, ok := ids[id]
		return ok
	}
}

// RunDelete removes saved sessions per --delete or --delete-old and returns
// the process exit code. Progress goes to out. In answers the sweep's confirmation.
func RunDelete(out io.Writer, in io.Reader, o DeleteOptions) int {
	store, err := session.NewStore()
	if err == nil {
		stderrNote(store.Note())
		cwd := config.Cwd()
		if o.Target != "" {
			err = session.DeleteSession(out, store, cwd, o.Target, o.Force, servedSessions())
		} else {
			cutoff := time.Now().UTC().AddDate(0, 0, -o.OldDays)
			err = session.DeleteOldSessions(out, in, store, cwd, cutoff, servedSessions())
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		return ExitUsage
	}
	return ExitOK
}
