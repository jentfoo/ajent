package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jentfoo/ajent/pkg/ctl"
	"github.com/jentfoo/ajent/pkg/paths"
)

// DirName holds session sockets under the ajent config dir. It must agree
// with the server's bind location in pkg/app.
const DirName = "control"

// Session is one live session's socket with the identity its hello announced.
type Session struct {
	Path string
	ID   string
	Name string
	PID  int
}

// String renders one listing row: id, name and pid as far as each is known.
func (s Session) String() string {
	name := s.Name
	if name == "" {
		name = "-"
	}
	return fmt.Sprintf("%s  %s  pid %d", s.ID, name, s.PID)
}

// Resolve turns an --attach-style target into a socket path: an existing file
// wins, then <session>.sock in the control dir, then a unique session id
// prefix. The returned list covers every live socket for the failure message.
func Resolve(target string) (string, []Session, error) {
	live, _ := Discover()
	if target == "" {
		return "", live, errors.New("no attach target given")
	}
	if fi, serr := os.Stat(target); serr == nil && fi.Mode()&os.ModeSocket != 0 {
		return target, live, nil
	}
	dir, derr := Dir()
	if derr != nil {
		return "", live, derr
	}
	direct := filepath.Join(dir, target+".sock")
	if fi, serr := os.Stat(direct); serr == nil && fi.Mode()&os.ModeSocket != 0 {
		return direct, live, nil
	}

	matches := slices.DeleteFunc(slices.Clone(live), func(s Session) bool {
		return !strings.HasPrefix(s.ID, target)
	})
	switch len(matches) {
	case 1:
		return matches[0].Path, live, nil
	case 0:
		return "", live, fmt.Errorf("no session socket at %s", target)
	default:
		return "", live, fmt.Errorf("%q matches %d sessions; give the full id or path", target, len(matches))
	}
}

// Dir returns <config>/control without creating it.
func Dir() (string, error) {
	home, err := paths.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, DirName), nil
}

// Discover probes every socket in the control dir and returns the live ones,
// sorted by session id. A peer that never says hello is skipped: nothing to
// attach to.
func Discover() ([]Session, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	names, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var out []Session
	for _, name := range names {
		path := filepath.Join(dir, name.Name())
		fi, serr := os.Stat(path)
		if serr != nil || fi.Mode()&os.ModeSocket == 0 {
			continue // a stale or foreign file is never attachable anyway
		}
		hello, err := probeHello(path)
		if err != nil {
			continue
		}
		out = append(out, Session{Path: path, ID: hello.Session.ID,
			Name: hello.Session.Name, PID: hello.PID})
	}
	slices.SortFunc(out, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// probeHello reads one socket's hello line.
func probeHello(path string) (hello ctl.Hello, err error) {
	conn, derr := (&net.Dialer{Timeout: CallWait}).Dial("unix", path)
	if derr != nil {
		return hello, derr
	}
	defer func() { _ = conn.Close(); _ = conn.SetDeadline(time.Time{}) }()
	if err = conn.SetDeadline(time.Now().Add(helloWait)); err != nil {
		return ctl.Hello{}, err
	}

	var raw json.RawMessage
	if err = ctl.NewDecoder(conn).Decode(&raw); err != nil {
		return ctl.Hello{}, err
	}
	in, derr := ctl.DecodeIncoming(raw)
	if derr != nil || in.Event == nil || in.Event.Name != ctl.EventHello {
		return ctl.Hello{}, errors.New("first control line was not a hello event")
	}
	if derr = in.Event.Decode(&hello); derr != nil {
		return ctl.Hello{}, derr
	}
	return hello, nil
}
