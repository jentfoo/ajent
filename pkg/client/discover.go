package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/paths"
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

// Resolve turns an --attach-style target into its live instances, with the
// same rules --resume resolves names by: an exact name first (case
// insensitive), then a full id or unique prefix. An existing socket path wins
// outright, the escape hatch tests and scripts use. The second slice covers
// every live instance, for failure messages and pickers.
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
	switch len(matches) {
	case 0:
		return nil, live, fmt.Errorf("no live control session matches %q", target)
	default:
		return matches, live, nil
	}
}

// matchInstances applies the resume rules over registry rows: exact name, then
// full id or unique prefix. An ambiguous prefix matches nothing and names the
// error at the caller.
func matchInstances(live []Instance, target string) []Instance {
	named := slices.DeleteFunc(slices.Clone(live), func(in Instance) bool {
		return in.SessionName == "" || !strings.EqualFold(in.SessionName, target)
	})
	if len(named) > 0 {
		return named
	}
	return slices.DeleteFunc(slices.Clone(live), func(in Instance) bool {
		return in.SessionID != target && !strings.HasPrefix(in.SessionID, target)
	})
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
