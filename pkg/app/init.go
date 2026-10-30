package app

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

// agentsFileName is the instruction file /init writes, and the survey watches its
// stamp so the "applies on next start" notice only follows a real write.
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
	// send hands one line to the prompt pump: it stamps the dispatch generation
	// and drops the line once the app root is canceled. nil disables /init.
	send func(pumpLine)
}

// initController owns the /init project survey: one at a time, cancellable, and
// handed back to the prompt pump as an ordinary turn once the survey is in hand.
type initController struct {
	deps   initDeps
	runner *projinit.Runner

	mu      sync.Mutex
	cancel  context.CancelFunc
	spawned []string // this survey's child ids, so an abort spares the model's own
	aborted bool     // abort ran: a late track stops its spawn instead of listing it
}

// newInitController returns the controller, or nil when there is no tool registry
// to run the survey through. In that case /init is simply absent.
func newInitController(deps initDeps) *initController {
	if deps.toolsReg == nil || deps.notify == nil || deps.send == nil {
		return nil
	}
	c := &initController{
		deps: deps,
	}
	c.runner = projinit.New(projinit.Options{
		Cwd:      deps.cwd,
		Registry: deps.toolsReg,
		Sink:     deps.sink,
		Notify:   func(msg string, level agent.Level) { deps.notify(msg, tui.Level(level)) },
		Started:  c.track,
	})
	return c
}

// track records a child this survey spawned. A spawn whose id lands after an
// abort missed the stop-by-id sweep, so it is stopped here instead.
func (c *initController) track(id string) {
	c.mu.Lock()
	stopped := c.aborted
	if !stopped {
		c.spawned = append(c.spawned, id)
	}
	c.mu.Unlock()

	if stopped && c.deps.agents != nil {
		_ = c.deps.agents.Stop(id) // already finished is not a failure here
	}
}

// start runs one survey on its own goroutine, so the pump keeps serving input for
// the minutes it takes. Refused while a turn streams or a survey is already up.
func (c *initController) start(ctx context.Context) {
	if c.deps.ag != nil && c.deps.ag.Running() {
		c.deps.notify("init refused: press Esc to stop the turn first", tui.LevelWarn)
		return
	}
	// the handoff rides the driver's send, so an abort (the WithCancel below)
	// must not silence it; only app-root cancellation drops the line
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		cancel()
		c.deps.notify("init: a survey is already running", tui.LevelWarn)
		return
	}
	c.cancel, c.spawned, c.aborted = cancel, nil, false
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
			c.submit(in)
		}
	}()
}

// submit hands the finished survey to the pump as an ordinary turn. The
// driver's send stamps it with the dispatch generation and drops it once the
// app root is canceled (pump shutdown).
func (c *initController) submit(in agent.Input) {
	c.deps.send(pumpLine{
		kind: command.KindPrompt, rest: "/init", input: &in, onTurn: c.armWatch,
	})
}

// abort ends a running survey and stops the children it spawned, reporting
// whether there was one to stop. A returning Poll does not end a job, so each is
// stopped by id, never StopAll. That would also kill investigations the model
// started for itself while the survey ran. A spawn still registering when this
// runs is stopped by track instead.
func (c *initController) abort() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	cancel, ids := c.cancel, c.spawned
	if cancel != nil { // nothing to mark once done has released the slot
		c.aborted = true
	}
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

// done releases the in-flight slot so a later /init may run. Every path out of
// start cancels its survey context, completion no less than abort.
func (c *initController) done() {
	c.mu.Lock()
	cancel := c.cancel
	c.cancel = nil
	if cancel != nil { // abort may have fired it already, a repeat is a no-op
		cancel() // held under the lock so release never outruns the unwind
	}
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
		Handler: func(ctx context.Context, _ string, _ command.Console) error {
			ctl.start(ctx)
			return nil
		},
	}}
}

// initWatch notices the turn that writes AGENTS.md, so the driver can say it only
// applies on the next start. Project instructions are read once at startup, so there
// is no mid-session reload.
type initWatch struct {
	agent.NopSink
	notify func(msg string, level tui.Level)

	mu   sync.Mutex
	path string
	prev fileStamp
}

// arm records the instruction file's current stamp ahead of a survey turn.
func (w *initWatch) arm(path string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	w.path, w.prev = path, stampOf(path)
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
	now := stampOf(path)
	if now.mod.IsZero() || now == prev {
		return // nothing written yet, keep watching
	}
	w.mu.Lock()
	w.path = ""
	w.mu.Unlock()
	if w.notify != nil {
		w.notify("init: "+agentsFileName+" written; it applies on the next start", tui.LevelInfo)
	}
}

// fileStamp is a watched file's identity. A write landing in the same tick it
// was armed under leaves mtime untouched on coarse-mtime filesystems, so the
// size moves with it.
type fileStamp struct {
	size int64
	mod  time.Time
}

// stampOf returns path's stamp, zero when it does not exist.
func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{size: fi.Size(), mod: fi.ModTime()}
}
