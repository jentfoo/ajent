package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// DeleteSession removes the one session target names, resolved the way --resume
// resolves it: an exact name, a full id, or a unique id prefix.
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
	_, _ = fmt.Fprintf(out, "Deleted session %s (%s).\n", infoLabel(info), describeInfo(info))
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
		_, _ = fmt.Fprintf(out, "  %s  %s\n", infoLabel(info), describeInfo(info))
	}
	_, _ = fmt.Fprintf(out, "Delete %s? [y/N] ", plural(len(stale), "session"))
	if !confirmed(in) {
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
	_, _ = fmt.Fprintf(out, "Deleted %s.\n", plural(removed, "session"))
	return errors.Join(errs...)
}

// confirmed reads one line and reports whether it agrees. EOF (no terminal, or
// stdin closed) declines rather than deleting blind.
func confirmed(in io.Reader) bool {
	sc := bufio.NewScanner(in)
	if !sc.Scan() {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(sc.Text()))
	return answer == "y" || answer == "yes"
}

// plural returns n with unit, suffixed beyond one.
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// infoLabel returns what --resume takes back for a saved session: its name, or
// its id when unnamed.
func infoLabel(info Info) string {
	if info.Name != "" {
		return info.Name
	}
	return info.ID
}

// describeInfo summarises a saved session for the delete output.
func describeInfo(info Info) string {
	return fmt.Sprintf("%s, last used %s",
		plural(info.Messages, "message"), info.Updated.Local().Format("2006-01-02")) // recorded in UTC, rendered in the system local time zone
}
