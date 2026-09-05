package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/session"
)

// defaultStaleDays is the --delete-old window when no day count is given.
const defaultStaleDays = 28

// extractDeleteOld lifts --delete-old and its optional trailing day count out of
// argv. A bare `--delete-old` means the default window.
func extractDeleteOld(argv []string) (given bool, days string, rest []string) {
	// only a day count is the value, so a stray token stays a positional validate
	// can name as a bad day count
	return extractOptional(argv, "delete-old", func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	})
}

// runDelete carries out --delete or --delete-old and returns the process exit
// code. Progress goes to out; in answers the --delete-old confirmation.
func runDelete(out io.Writer, in io.Reader, f cliFlags) int {
	store, err := session.NewStore()
	if err == nil {
		cwd := cwdOrDot()
		if f.deleteGiven {
			err = deleteSession(out, store, cwd, f.deleteTarget)
		} else {
			cutoff := time.Now().UTC().AddDate(0, 0, -f.deleteOldDays)
			err = deleteOldSessions(out, in, store, cwd, cutoff)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ajent: %v\n", err)
		return exitUsage
	}
	return exitOK
}

// deleteSession removes the one session target names, resolved the way --resume
// resolves it: an exact name, a full id, or a unique id prefix.
func deleteSession(out io.Writer, store *session.Store, cwd, target string) error {
	info, err := store.Find(cwd, target)
	if errors.Is(err, session.ErrNotFound) {
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

// deleteOldSessions removes every unnamed session last used before cutoff, after
// listing them and reading a confirmation from in. An unreadable answer declines.
func deleteOldSessions(out io.Writer, in io.Reader, store *session.Store, cwd string, cutoff time.Time) error {
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
func infoLabel(info session.Info) string {
	if info.Name != "" {
		return info.Name
	}
	return info.ID
}

// describeInfo summarises a saved session for the delete output.
func describeInfo(info session.Info) string {
	return fmt.Sprintf("%s, last used %s",
		plural(info.Messages, "message"), info.Updated.Format("2006-01-02")) // sessions are stored as UTC
}
