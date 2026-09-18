package app

import (
	"os/exec"
	"slices"
	"strings"

	"github.com/go-analyze/bulk"
	"github.com/jentfoo/ajent/pkg/config"
	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tools"
)

// advertisedCommands merges the fixed probe set with tools.shellCommands extras,
// keeping unique bare names on PATH that permissions.deniedCommands leaves
// unquestioned and no enabled tool already covers. It also returns the names
// missing from PATH, for the caller to surface. The result is startup-fixed.
func advertisedCommands(settings config.Settings, lookPath func(string) (string, error)) ([]string, []string) {
	denied := settings.Permissions.DeniedCommands
	if slices.Contains(denied, tools.ToolBash) { // bash itself denied: advertise nothing
		return nil, nil
	}

	merged := slices.Concat(tools.ShellExamples, settings.Tools.ShellCommands)
	for i, cmd := range merged {
		merged[i] = strings.TrimSpace(cmd)
	}
	candidates := bulk.SliceFilter(func(cmd string) bool { return cmd != "" }, merged)
	candidates = bulk.SliceDifference(candidates, nil) // dedupe keeping first occurrence

	var missing []string
	kept := bulk.SliceFilter(func(cmd string) bool {
		if slices.Contains(settings.Tools.Enabled, cmd) {
			return false
		}
		if _, err := lookPath(cmd); err != nil {
			missing = append(missing, cmd)
			return false
		}
		return !permit.CommandRefused(cmd, denied)
	}, candidates)
	return kept, missing
}

// builtinTools builds the built-in registry shared by interactive and headless
// runs, surfacing shellCommands entries missing from PATH through warn. The
// registry backs read's live vision gate, so both must be the same object.
func builtinTools(set *config.Set, reg *llm.Registry, ask tools.AskFunc, warn func(string)) (*tools.Registry, error) {
	cmds, missing := advertisedCommands(set.Settings(), exec.LookPath)
	if len(missing) > 0 && warn != nil {
		warn("tools.shellCommands: not on PATH: " + strings.Join(missing, ", "))
	}
	return tools.Builtins(tools.Options{
		SessionID:     config.Cwd(),
		Ask:           ask,
		ShellCommands: cmds,
		Vision:        func() bool { return reg.Active().Caps.Images },
	})
}
