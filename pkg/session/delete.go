package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/go-analyze/bulk"

	"github.com/jentfoo/ajent/pkg/strutil"
)

// DeleteSession removes the one session target names, resolved the way --resume
// resolves it: an exact name, a full id or unique prefix, or a filesystem path
// onto the workspace's session directory. A newer-format session is removable
// too: deletion needs no decode, and it is the escape hatch that frees a name a
// blocked transcript would otherwise hold forever. The same path handle removes
// a damaged transcript whose identity cannot decode, which resolution reaches
// by nothing else. A session another writer holds live refuses naming the
// instance count unless force is set.
func DeleteSession(out io.Writer, store *Store, cwd, target string, force bool) error {
	info, err := store.Find(cwd, target)
	if err != nil { // the not-found wording (with any damaged hint) passes through verbatim
		return err
	}
	if live := LiveWriters(info.Path); live > 0 && !force {
		return fmt.Errorf("session %s is live in %d running ajent instance(s); detach or pass --force",
			info.Label(), live)
	}
	if err := store.Remove(info.Path); err != nil {
		return err
	}
	if info.Blocked {
		_, _ = fmt.Fprintf(out, "Deleted session %s (newer session format).\n", info.Label())
		return nil
	}
	if info.Damaged {
		// no identity to label it with: the path is both the handle and the receipt
		_, _ = fmt.Fprintf(out, "Deleted damaged transcript %s.\n", info.Path)
		return nil
	}
	_, _ = fmt.Fprintf(out, "Deleted session %s (%s).\n", info.Label(), describeInfo(info))
	return nil
}

// DeleteOldSessions removes every unnamed session last used before cutoff, after
// listing them and reading a confirmation from in. An unreadable answer declines.
// Sessions a writer holds live are listed as skipped rather than deleted, and
// re-checked at Remove time so a session that went live while the user read the
// list never loses its file.
func DeleteOldSessions(out io.Writer, in io.Reader, store *Store, cwd string, cutoff time.Time) error {
	stale, err := store.Stale(cwd, cutoff)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		_, _ = fmt.Fprintln(out, "No unnamed sessions are old enough to delete.")
		return nil
	}
	// liveness is read once per row: a second pass could drop a session that
	// went live between the reads from both lists
	type oldRow struct {
		info Info
		live bool
	}
	rows := make([]oldRow, 0, len(stale))
	for _, info := range stale {
		rows = append(rows, oldRow{info: info, live: LiveWriters(info.Path) > 0})
	}
	live := bulk.SliceFilter(func(r oldRow) bool { return r.live }, rows)
	removable := bulk.SliceFilter(func(r oldRow) bool { return !r.live }, rows)
	for _, r := range live {
		_, _ = fmt.Fprintf(out, "  %s  %s (live, skipped)\n", r.info.Label(), describeInfo(r.info))
	}
	if len(removable) == 0 {
		_, _ = fmt.Fprintln(out, "Nothing to delete; every old session is live.")
		return nil
	}
	for _, r := range removable {
		_, _ = fmt.Fprintf(out, "  %s  %s\n", r.info.Label(), describeInfo(r.info))
	}
	_, _ = fmt.Fprintf(out, "Delete %s? [y/N] ", strutil.Plural(len(removable), "session"))
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
	for _, r := range removable {
		if LiveWriters(r.info.Path) > 0 { // went live between the listing and here
			_, _ = fmt.Fprintf(out, "Skipped %s (now live).\n", r.info.Label())
			continue
		}
		// one bad file must not strand the rest of the sweep
		if rerr := store.Remove(r.info.Path); rerr != nil {
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
