package agent

import (
	"context"
	"sync"
	"time"

	"github.com/jentfoo/ajent/pkg/llm"
	"github.com/jentfoo/ajent/pkg/tokens"
)

// defaultMaxSteps caps a single turn's tool-calling iterations so a runaway
// loop cannot spin forever.
const defaultMaxSteps = 100

// CompactReason tells the compact hook why it was asked to reduce context.
type CompactReason uint8

const (
	CompactManual    CompactReason = iota // /compact; the caller asks directly, not via the hook
	CompactThreshold                      // a turn boundary; the hook decides whether to act
	CompactOverflow                       // a request exceeded the window and must shrink before retry
)

// Options configures an Agent.
type Options struct {
	Provider            func(llm.Model) (llm.Provider, error) // resolved per request so /model switching works
	Sinks               []Sink                                // fanned out in registration order
	Tools               ToolSet                               // nil disables tool calling entirely
	Env                 Environment                           // OS facts layered into the system prompt
	ProjectInstructions []ProjectInstruction                  // AGENTS.md content; loaded once at startup
	Transforms          []Transform                           // applied in assembly order, nil entries skipped
	OnMessage           []func(MessageInfo)                   // called per appended message, in registration order
	OnSettled           []func(context.Context)               // agent drained and idle; observers may queue work
	// Compact reduces the live context at a turn boundary or after an overflow,
	// reporting whether anything changed. It never runs mid-stream.
	Compact   func(ctx context.Context, r CompactReason) (bool, error)
	MaxSteps  int    // defaults to defaultMaxSteps
	SessionID string // session-affinity headers on requests that support them
}

// Agent runs turns against a model provider, streaming deltas to the sink and
// dispatching tool calls until the model stops. Turn state lives here so
// callers can observe turns and inject steering.
type Agent struct {
	opts  Options
	state *State
	sink  Sink // resolved once from opts.Sinks so runTurn reads one field

	ctxLast   int       // last emitted Used, for throttling Context emits
	ctxLastAt time.Time // when that emit happened; drives the interval throttle

	mu       sync.Mutex
	running  bool
	settling int // depth of OnSettled notification; observers may queue work while >0
	steer    []Input
	follow   []Input
	cancel   context.CancelFunc
}

// New returns an agent bound to state. Sinks are resolved once into a single
// fan-out so the loop always emits on one field; with none supplied events go
// nowhere.
func New(state *State, opts Options) *Agent {
	if opts.MaxSteps == 0 {
		opts.MaxSteps = defaultMaxSteps
	}
	a := &Agent{state: state, opts: opts}
	switch len(opts.Sinks) {
	case 0:
		a.sink = NopSink{}
	case 1:
		a.sink = opts.Sinks[0]
	default:
		a.sink = &fanoutSink{sinks: opts.Sinks}
	}
	return a
}

// Running reports whether a turn is in flight. It is advisory for steering, not
// a lock; callers that need ordering use Prompt's completion.
func (a *Agent) Running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

// Steer queues input to be injected into the running turn at its next step
// boundary, or reports false when idle. It does not cancel the in-flight model
// call; FollowUp is for the impatient case.
func (a *Agent) Steer(in Input) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running && a.settling == 0 { // an OnSettled observer may queue work
		return false
	}
	a.steer = append(a.steer, in)
	return true
}

// FollowUp queues input as the next turn once the running one settles, or
// reports false when idle (call Prompt instead).
func (a *Agent) FollowUp(in Input) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running && a.settling == 0 { // an OnSettled observer may queue work
		return false
	}
	a.follow = append(a.follow, in)
	return true
}

// Interrupt cancels the running turn and drops anything queued. It is safe to
// call at any time; an idle agent ignores it.
func (a *Agent) Interrupt() {
	a.mu.Lock()
	cancel := a.cancel
	a.steer = nil
	a.follow = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Prompt runs input to completion, including any follow-up queued while it ran.
// It returns when both queues are empty or the context ends.
func (a *Agent) Prompt(ctx context.Context, in Input) error {
	return a.runTurns(ctx, []Input{in})
}

// WithState runs fn against the live state, reporting false when a turn is
// running. Rewind and compaction both rewrite context this way, so every holder
// of *State observes the change.
func (a *Agent) WithState(fn func(*State)) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return false
	}
	fn(a.state)
	return true
}

// Recount replaces the context estimate with an exact count from the provider's
// tokenizer, or returns llm.ErrNoTokenizer when it has none. It refuses while a
// turn is running, like ResetState.
func (a *Agent) Recount(ctx context.Context) (int, error) {
	a.mu.Lock()
	if a.running {
		a.mu.Unlock()
		return 0, errTurnRunning
	}
	a.mu.Unlock()
	return a.recount(ctx)
}

// recount replaces the ledger's estimate with the provider tokenizer's exact count
// of what buildRequest would send. It is shared by the external Recount and the
// turn loop (which calls it while running, so it must not take the agent lock).
func (a *Agent) recount(ctx context.Context) (int, error) {
	p, err := a.opts.Provider(a.state.Model)
	if err != nil {
		return 0, err
	}
	n, err := tokens.Recount(ctx, p, a.buildRequest())
	if err != nil {
		return 0, err
	}
	if t := a.state.Tokens; t != nil {
		t.Rebase(n)
	}
	return n, nil
}
