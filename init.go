package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/agent"
	"github.com/jentfoo/ajent/pkg/command"
	"github.com/jentfoo/ajent/pkg/projinit"
	"github.com/jentfoo/ajent/pkg/subagent"
	"github.com/jentfoo/ajent/pkg/tools"
	"github.com/jentfoo/ajent/pkg/tui"
)

// agentsFileName is the instruction file /init writes; the survey watches its
// mtime so the "applies on next start" notice only follows a real write.
const agentsFileName = "AGENTS.md"

// initDeps are the driver objects the /init controller closes over.
type initDeps struct {
	cwd      string
	toolsReg *tools.Registry
	sink     agent.Sink
	ag       *agent.Agent
	notify   func(msg string, level tui.Level)
	agents   *subagent.Manager
	watch    *initWatch
	pump     chan<- pumpLine
}

// initController owns the /init project survey: one at a time, cancellable, and
// handed back to the prompt pump as an ordinary turn once the survey is in hand.
type initController struct {
	deps   initDeps
	runner *projinit.Runner

	mu      sync.Mutex
	cancel  context.CancelFunc
	spawned []string // this survey's child ids, so an abort spares the model's own
}

// newInitController returns the controller, or nil when there is no tool registry
// to run the survey through — /init is then simply absent.
func newInitController(deps initDeps) *initController {
	if deps.toolsReg == nil || deps.notify == nil || deps.pump == nil {
		return nil
	}
	c := &initController{
		deps: deps,
	}
	c.runner = projinit.New(projinit.Options{
		Cwd:      deps.cwd,
		Registry: deps.toolsReg,
		Sink:     deps.sink,
		Notify:   func(msg string, level agent.Level) { deps.notify(msg, tuiLevel(level)) },
		Started:  c.track,
	})
	return c
}

// track records a child this survey spawned.
func (c *initController) track(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spawned = append(c.spawned, id)
}

// start runs one survey on its own goroutine, so the pump keeps serving input for
// the minutes it takes. Refused while a turn streams or a survey is already up.
func (c *initController) start() {
	if c.deps.ag != nil && c.deps.ag.Running() {
		c.deps.notify("init refused: press Esc to stop the turn first", tui.LevelWarn)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		cancel()
		c.deps.notify("init: a survey is already running", tui.LevelWarn)
		return
	}
	c.cancel, c.spawned = cancel, nil
	c.mu.Unlock()

	go func() {
		defer c.done()
		in, err := c.runner.Survey(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			c.deps.notify("init: survey cancelled", tui.LevelWarn)
		case err != nil:
			c.deps.notify("init: "+err.Error(), tui.LevelWarn)
		default:
			// the label is what a queued row shows and what Esc recovers into the
			// editor; re-submitting it simply runs /init again.
			c.deps.pump <- pumpLine{
				kind: command.KindPrompt, rest: "/init", input: &in, onTurn: c.armWatch,
			}
		}
	}()
}

// abort ends a running survey and stops the children it spawned, reporting
// whether there was one to stop. A returning Poll does not end a job, so each is
// stopped by id — never StopAll, which would also kill investigations the model
// started for itself while the survey ran.
func (c *initController) abort() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	cancel, ids := c.cancel, c.spawned
	c.spawned = nil
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	if c.deps.agents != nil {
		for _, id := range ids {
			_ = c.deps.agents.Stop(id) // already finished is not a failure here
		}
	}
	return true
}

// done releases the in-flight slot so a later /init may run.
func (c *initController) done() {
	c.mu.Lock()
	c.cancel = nil
	c.mu.Unlock()
}

// armWatch marks the instruction file ahead of the survey's turn. The pump calls
// it, not the survey goroutine: a turn already running when the survey finished
// would otherwise end first and consume the arming.
func (c *initController) armWatch() {
	if c == nil {
		return
	}
	c.deps.watch.arm(filepath.Join(c.deps.cwd, agentsFileName))
}

// initCommands is /init, registered alongside the built-ins rather than in place
// of them.
func initCommands(ctl *initController) []command.Command {
	if ctl == nil {
		return nil
	}
	return []command.Command{{
		Name:        "init",
		Description: "survey the project and write AGENTS.md",
		Handler: func(_ context.Context, _ string, _ command.Console) error {
			ctl.start()
			return nil
		},
	}}
}

// initWatch notices the turn that writes AGENTS.md, so the driver can say it only
// applies on the next start. Project instructions are read once at startup; there
// is no mid-session reload.
type initWatch struct {
	agent.NopSink
	notify func(msg string, level tui.Level)

	mu   sync.Mutex
	path string
	prev time.Time
}

// arm records the instruction file's current mtime ahead of a survey turn.
func (w *initWatch) arm(path string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.path, w.prev = path, modTime(path)
}

// TurnEnd reports the file once it has actually changed, then disarms. It stays
// armed across turns that did not touch it: a survey queued behind a running turn
// is written by a later one, and disarming on the first turn end lost the notice.
// A denied write therefore says nothing rather than saying the wrong thing.
func (w *initWatch) TurnEnd(agent.TurnResult) {
	w.mu.Lock()
	path, prev := w.path, w.prev
	w.mu.Unlock()
	if path == "" {
		return
	}
	now := modTime(path)
	if now.IsZero() || now.Equal(prev) {
		return // nothing written yet; keep watching
	}
	w.mu.Lock()
	w.path = ""
	w.mu.Unlock()
	if w.notify != nil {
		w.notify("init: "+agentsFileName+" written; it applies on the next start", tui.LevelInfo)
	}
}

// modTime returns path's modification time, zero when it does not exist.
func modTime(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
