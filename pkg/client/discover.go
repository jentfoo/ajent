package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/paths"
	"github.com/jentfoo/ajent/pkg/session"
)

// Instance is one live session's serving instance, read from the registry file
// beside its socket. Discovery never dials: every field was written by the
// instance after listen, and the one remaining dial is the final connect.
type Instance = ctl.Instance

// Dir returns <config>/<control socket dir> without creating it.
func Dir() (string, error) {
	home, err := paths.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, paths.ControlDirName), nil
}

// Discover reads the registry: every live instance's record, sorted by session
// id. A corrupt or truncated file reads as absent, and nothing here dials, so
// discovery costs one directory read.
func Discover() ([]Instance, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	sessions, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Instance
	for _, dirEntry := range sessions {
		if !dirEntry.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dir, dirEntry.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(dir, dirEntry.Name(), f.Name()))
			if err != nil {
				continue
			}
			var in Instance
			if json.Unmarshal(b, &in) != nil || in.SessionID == "" || in.Socket == "" {
				continue
			}
			out = append(out, in)
		}
	}
	slices.SortFunc(out, func(a, b Instance) int { return strings.Compare(a.SessionID, b.SessionID) })
	return out, nil
}

// Resolve turns an --attach-style target into its live instances, reading
// names like --resume does: an exact session name first (case insensitive),
// then a full id or any prefix of one; several matches all return, for the
// caller to pick among. An existing socket path wins outright, the escape
// hatch tests and scripts use. The second slice covers every live instance,
// for failure messages and pickers.
func Resolve(target string) ([]Instance, []Instance, error) {
	live, err := Discover()
	if err != nil {
		return nil, nil, err
	}
	if target == "" {
		return live, live, nil
	}
	if fi, serr := os.Stat(target); serr == nil && fi.Mode()&os.ModeSocket != 0 {
		direct := []Instance{{Socket: target}}
		return direct, live, nil
	}
	matches := matchInstances(live, target)
	if len(matches) == 0 {
		return nil, live, fmt.Errorf("no live control session matches %q", target)
	}
	return matches, live, nil
}

// matchInstances applies the resume name rules over registry rows: exact
// name first (case insensitive), then full id or prefix. Ambiguity is not an
// error here; every match returns for the caller to pick among.
func matchInstances(live []Instance, target string) []Instance {
	named := bulk.SliceFilter(func(in Instance) bool {
		return in.SessionName != "" && strings.EqualFold(in.SessionName, target)
	}, live)
	if len(named) > 0 {
		return named
	}
	return bulk.SliceFilter(func(in Instance) bool {
		return in.SessionID == target || strings.HasPrefix(in.SessionID, target)
	}, live)
}

// Followable picks the instance a reconnecting controller follows: the same
// token when it still serves, else the one remaining instance of the session
// (the restarted-serve case). Several remaining answers nil, never a silent
// swap onto a sibling's head.
func Followable(live []Instance, sessionID, token string) []Instance {
	ofSession := bulk.SliceFilter(func(in Instance) bool { return in.SessionID == sessionID }, live)
	if i := slices.IndexFunc(ofSession, func(in Instance) bool { return in.Token == token }); i >= 0 {
		return ofSession[i : i+1]
	}
	if len(ofSession) == 1 {
		return ofSession
	}
	return nil
}

// staleAfter is how old a record must be before its pid is judged, the same
// minute the serving side's passive sweep grants a writer to finish starting.
const staleAfter = time.Minute

// InstanceDead reports whether a discovery record describes an instance no
// longer running: the record is at least one minute old and signal zero on its
// recorded pid proves no such process. Judged from the record alone, never by
// dialing, so a picker costs nothing to filter. Anything undecidable reads as
// alive (no usable pid, a permission error, a reused pid), so a caller only
// ever drops a record on proof of death.
func InstanceDead(in Instance) bool {
	if time.Since(in.Started()) < staleAfter {
		return false // fresh: its writer may still be binding
	}
	return pidDead(in.PID)
}

// pidDead reports whether no process with pid exists, judged with signal zero:
// it delivers nothing, EPERM means a live process this user cannot signal (a
// PID reuse by another user), and anything undecidable reads as alive. The os
// layer folds ESRCH into os.ErrProcessDone, so both spellings of "no such
// process" count.
func pidDead(pid int) bool {
	if pid <= 0 {
		return false // no recorded pid: nothing to judge, leave the record
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone)
}

// DiscoverLive is Discover without the dead records: every instance whose
// recorded pid still answers signal zero past its first minute. It never dials
// and never collects; removing a dead record stays the serving instances'
// passive sweep.
func DiscoverLive() ([]Instance, error) {
	all, err := Discover()
	if err != nil {
		return nil, err
	}
	return bulk.SliceFilterInPlace(func(in Instance) bool { return !InstanceDead(in) }, all), nil
}

// SavedSessions lists every saved session under ajent's home across all
// workspaces, most recently used first: the store's own cross-workspace
// listing (session.Store.ListAll), surfaced here so a controller needs no
// other import to reach it. Each row names the workspace its transcript
// recorded for resume targets to derive from; opening the store creates the
// sessions root when missing.
func SavedSessions() ([]session.Listed, error) {
	store, err := session.NewStore()
	if err != nil {
		return nil, err
	}
	return store.ListAll()
}
