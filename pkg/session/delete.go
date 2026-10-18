package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// DeleteSession removes the one session target names, resolved the way --resume
// resolves it: an exact name, a full id, or a unique id prefix. A newer-format
// session is removable too: deletion needs no decode, and it is the escape
// hatch that frees a name a blocked transcript would otherwise hold forever.
func DeleteSession(out io.Writer, store *Store, cwd, target string) error {
	info, err := store.Find(cwd, target)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("no session matches %q", target)
	} else if err != nil {
		return err
	}
	if err := store.Remove(info.Path); err != nil {
		return err
	}
	if info.Blocked {
		_, _ = fmt.Fprintf(out, "Deleted session %s (newer session format).\n", info.Label())
		return nil
	}
	_, _ = fmt.Fprintf(out, "Deleted session %s (%s).\n", info.Label(), describeInfo(info))
	return nil
}

// DeleteOldSessions removes every unnamed session last used before cutoff, after
// listing them and reading a confirmation from in. An unreadable answer declines.
func DeleteOldSessions(out io.Writer, in io.Reader, store *Store, cwd string, cutoff time.Time) error {
	stale, err := store.Stale(cwd, cutoff)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		_, _ = fmt.Fprintln(out, "No unnamed sessions are old enough to delete.")
		return nil
	}
	for _, info := range stale {
		_, _ = fmt.Fprintf(out, "  %s  %s\n", info.Label(), describeInfo(info))
	}
	_, _ = fmt.Fprintf(out, "Delete %s? [y/N] ", strutil.Plural(len(stale), "session"))
	// EOF (no terminal, or stdin closed) declines rather than deleting blind
	sc := bufio.NewScanner(in)
	answer := ""
	if sc.Scan() {
		answer = strings.ToLower(strings.TrimSpace(sc.Text()))
	}
	if answer != "y" && answer != "yes" {
		_, _ = fmt.Fprintln(out, "Cancelled; nothing was deleted.")
		return nil
	}
	var errs []error
	var removed int
	for _, info := range stale {
		// one bad file must not strand the rest of the sweep
		if rerr := store.Remove(info.Path); rerr != nil {
			errs = append(errs, rerr)
		} else {
			removed++
		}
	}
	_, _ = fmt.Fprintf(out, "Deleted %s.\n", strutil.Plural(removed, "session"))
	return errors.Join(errs...)
}

// describeInfo summarises a saved session for the delete output.
func describeInfo(info Info) string {
	return fmt.Sprintf("%s, last used %s",
		strutil.Plural(info.Messages, "message"), info.Updated.Local().Format("2006-01-02")) // recorded in UTC, rendered in the system local time zone
}
