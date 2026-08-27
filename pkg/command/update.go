package command

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jentfoo/ajent/pkg/config"
)

// modulePath is the ajent module, reinstalled from @latest on update.
const modulePath = "github.com/jentfoo/ajent@latest"

// UpdateResult describes one completed self-update attempt. Exactly one of
// Latest or Err being set distinguishes the three outcomes: up to date,
// installed a newer version, or failed.
type UpdateResult struct {
	Current   string // build version before the attempt (config.Version)
	Latest    string // resolved @latest version; "" when resolution failed
	Installed bool   // true only after go install succeeded with a newer version
	Err       error  // non-nil on any resolve or install failure
}

// SelfUpdate resolves the latest published ajent and, when it differs from the
// running build, reinstalls via `go install`. It never panics; failures come back
// in Err. The current version is config.Version.
func SelfUpdate(ctx context.Context) UpdateResult {
	return selfUpdateWith(ctx, config.Version, updateCmds{})
}

// updateCmds are the two external commands a self-update runs; zero values use
// the real `go` invocations. Tests swap them for fakes to avoid network and PATH.
type updateCmds struct {
	resolve func(context.Context) (string, error)
	install func(context.Context) error
}

func (u updateCmds) resolveLatest(ctx context.Context) (string, error) {
	if u.resolve != nil {
		return u.resolve(ctx)
	}
	out, err := exec.CommandContext(ctx, "go", "list", "-m",
		"-f={{.Version}}", modulePath).Output()
	if err != nil {
		return "", fmt.Errorf("resolve latest: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (u updateCmds) installLatest(ctx context.Context) error {
	if u.install != nil {
		return u.install(ctx)
	}
	out, err := exec.CommandContext(ctx, "go", "install", modulePath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("go install: %w%s", err, note(out))
	}
	return nil
}

// selfUpdateWith is the pure decision core SelfUpdate wraps; current and cmds are
// injectable so tests exercise every branch without a real go toolchain.
func selfUpdateWith(ctx context.Context, current string, cmds updateCmds) UpdateResult {
	if current == "" {
		current = config.Version
	}
	latest, err := cmds.resolveLatest(ctx)
	if err != nil {
		return UpdateResult{Current: current, Err: err}
	}
	if latest == "" || latest == current {
		return UpdateResult{Current: current, Latest: latest}
	}
	if err := cmds.installLatest(ctx); err != nil {
		return UpdateResult{Current: current, Latest: latest, Err: err}
	}
	return UpdateResult{Current: current, Latest: latest, Installed: true}
}

// note returns a short suffix of command output for error context.
func note(out []byte) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return ": " + s
	}
	return ""
}
