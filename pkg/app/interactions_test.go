package app

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/jentfoo/ajent/pkg/permit"
	"github.com/jentfoo/ajent/pkg/tui"
)

// scriptedLocal answers every local dialog with canned results, recording the
// prompts it was asked. Mode reports alt render, like a real terminal.
type scriptedLocal struct {
	mu      sync.Mutex
	picks   []int
	multi   [][]int
	selects []int
	inputs  []string
	asks    []tui.Answer
	errs    []error // per-call errors, consumed in order

	asked []string
	mode  tui.Mode
}

func (s *scriptedLocal) Mode() tui.Mode { return s.mode }

func (s *scriptedLocal) nextErr() error {
	if len(s.errs) == 0 {
		return nil
	}
	e := s.errs[0]
	s.errs = s.errs[1:]
	return e
}

func (s *scriptedLocal) PickContext(_ context.Context, prompt string, _ []tui.PickItem, _ tui.PickOptions) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "pick:"+prompt)
	if err := s.nextErr(); err != nil {
		return 0, err
	}
	i := s.picks[0]
	s.picks = s.picks[1:]
	return i, nil
}

func (s *scriptedLocal) MultiPickContext(_ context.Context, prompt string, _ []tui.PickItem, _ tui.MultiPickOptions) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "multipick:"+prompt)
	if err := s.nextErr(); err != nil {
		return nil, err
	}
	out := s.multi[0]
	s.multi = s.multi[1:]
	return out, nil
}

func (s *scriptedLocal) SelectContext(_ context.Context, prompt string, _ []tui.Option) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "select:"+prompt)
	if err := s.nextErr(); err != nil {
		return 0, err
	}
	i := s.selects[0]
	s.selects = s.selects[1:]
	return i, nil
}

func (s *scriptedLocal) ConfirmContext(ctx context.Context, prompt string) (bool, error) {
	i, err := s.SelectContext(ctx, prompt, nil)
	return i == 0, err
}

func (s *scriptedLocal) InputContext(_ context.Context, label, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "input:"+label)
	if err := s.nextErr(); err != nil {
		return "", err
	}
	v := s.inputs[0]
	s.inputs = s.inputs[1:]
	return v, nil
}

func (s *scriptedLocal) InputSecretContext(ctx context.Context, label, placeholder string) (string, error) {
	return s.InputContext(ctx, label, placeholder)
}

func (s *scriptedLocal) Ask(_ context.Context, q tui.Question) (tui.Answer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, "ask:"+q.Text)
	if err := s.nextErr(); err != nil {
		return tui.Answer{}, err
	}
	a := s.asks[0]
	s.asks = s.asks[1:]
	return a, nil
}

// OpenDecision returns a handle whose Wait reports ErrNoUI, the shape of a
// local surface that cannot show an approval dialog.
func (s *scriptedLocal) OpenDecision(tui.DecisionRequest) *tui.Decision {
	return &tui.Decision{}
}

// log records Begin/End traffic for assertions.
type dialogLog struct {
	mu     sync.Mutex
	begins []interactionInfo
	ends   []endRecord
}

type endRecord struct {
	id      uint64
	outcome interactionOutcome
}

func (l *dialogLog) record() (func(interactionInfo), func(uint64, interactionOutcome)) {
	return func(i interactionInfo) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.begins = append(l.begins, i)
		}, func(id uint64, o interactionOutcome) {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.ends = append(l.ends, endRecord{id: id, outcome: o})
		}
}

func newBroker(ui localDialogs) (*interactionBroker, *dialogLog) {
	l := &dialogLog{}
	onBegin, onEnd := l.record()
	return newInteractionBroker(ui, onBegin, onEnd), l
}

// beginsSeen and endsSeen return recorded traffic snapshots.
func (l *dialogLog) beginsSeen() []interactionInfo {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.begins)
}

func (l *dialogLog) endsSeen() []endRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.ends)
}

func TestBrokerResolvesLocally(t *testing.T) {
	t.Parallel()

	local := &scriptedLocal{
		picks:   []int{2},
		multi:   [][]int{{0, 2}},
		selects: []int{1},
		inputs:  []string{"free text", "hunter2"},
		asks:    []tui.Answer{{Index: 0}},
	}
	b, log := newBroker(local)

	i, err := b.Pick(t.Context(), "Models", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, i)

	idx, err := b.MultiPick(t.Context(), "Tools", nil, tui.MultiPickOptions{})
	require.NoError(t, err)
	assert.Equal(t, []int{0, 2}, idx)

	sel, err := b.Select(t.Context(), "Choose", []tui.Option{{Label: "x"}, {Label: "y"}})
	require.NoError(t, err)
	assert.Equal(t, 1, sel)

	in, err := b.Input(t.Context(), "Name", "")
	require.NoError(t, err)
	assert.Equal(t, "free text", in)

	sec, err := b.InputSecret(t.Context(), "API key", "")
	require.NoError(t, err)
	assert.Equal(t, "hunter2", sec)

	ans, err := b.Ask(t.Context(), tui.Question{Text: "which?", Options: []tui.Option{{Label: "a"}}})
	require.NoError(t, err)
	assert.Equal(t, 0, ans.Index)

	// every dialog announced exactly what it asked and settled once
	require.Eventually(t, func() bool {
		return len(log.endsSeen()) == 6
	}, 2*time.Second, 5*time.Millisecond)
	begins := log.beginsSeen()
	require.Len(t, begins, 6)
	assert.Equal(t, dialogPick, begins[0].Kind)
	assert.Equal(t, []string{"a", "b"}, begins[0].Shapes.options)
	assert.Equal(t, dialogSecret, begins[4].Kind)
	// the secret's outcome never carries its text
	ends := log.endsSeen()
	assert.Empty(t, ends[4].outcome.Text)
	assert.True(t, ends[4].outcome.Answered)
}

func TestBrokerNoLocalHoldsForRemote(t *testing.T) {
	t.Parallel()

	b, log := newBroker(nil)
	resolved := make(chan error)
	go func() {
		_, err := b.Select(t.Context(), "Proceed?", []tui.Option{{Label: "Yes"}, {Label: "No"}})
		resolved <- err
	}()

	// one Begin announcing the dialog, no End yet
	require.Eventually(t, func() bool {
		return len(log.beginsSeen()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	assert.Equal(t, "select", log.beginsSeen()[0].Kind.String())

	require.NoError(t, b.Resolve(1, interactionAnswer{Index: 1}))
	require.NoError(t, <-resolved)

	require.Eventually(t, func() bool {
		return len(log.endsSeen()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	ends := log.endsSeen()
	assert.True(t, ends[0].outcome.Answered)
	assert.Equal(t, 1, ends[0].outcome.Index)
}

// a remote answer must cancel the local racer, or its menu stays open on the
// host terminal: /model answered from an attach left the picker up there.
func TestBrokerRemoteAnswerClosesLocal(t *testing.T) {
	t.Parallel()

	local := &blockingPick{cancelSeen: make(chan error, 1)}
	b, log := newBroker(local)

	picked := make(chan int, 1)
	go func() {
		i, err := b.Pick(t.Context(), "Model", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
		if err != nil {
			picked <- -1
			return
		}
		picked <- i
	}()

	require.Eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.open) == 1 // the local pick is up and holding for input
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, b.Resolve(firstOpenID(b), interactionAnswer{Index: 1}))
	assert.Equal(t, 1, <-picked)

	select {
	case err := <-local.cancelSeen:
		require.ErrorIs(t, err, context.Canceled) // the local dialog was dismissed
	case <-time.After(2 * time.Second):
		t.Fatal("remote answer did not cancel the open local pick")
	}

	require.Eventually(t, func() bool {
		return len(log.endsSeen()) == 1
	}, 2*time.Second, 5*time.Millisecond)
}

func TestBrokerFirstResolutionWins(t *testing.T) {
	t.Parallel()

	b, _ := newBroker(nil)
	resolved := make(chan int, 1)
	go func() {
		i, err := b.Pick(t.Context(), "Pick", []tui.PickItem{{Label: "a"}, {Label: "b"}}, tui.PickOptions{})
		if err != nil {
			resolved <- -1
			return
		}
		resolved <- i
	}()

	require.Eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.open) == 1
	}, 2*time.Second, 5*time.Millisecond)

	var wg sync.WaitGroup
	winners := make(chan error, 2)
	for _, idx := range []int{0, 1} {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			winners <- b.Resolve(1, interactionAnswer{Index: idx})
		}(idx)
	}
	wg.Wait()
	close(winners)

	var wins, losses int
	for err := range winners {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrInteractionSettled)
			losses++
		}
	}
	assert.Equal(t, 1, wins)
	assert.Equal(t, 1, losses)
	won := <-resolved
	assert.Contains(t, []int{0, 1}, won)
}

func TestBrokerEndIdempotent(t *testing.T) {
	t.Parallel()

	b, log := newBroker(nil)
	go func() {
		_, _ = b.Input(t.Context(), "Name", "")
	}()
	require.Eventually(t, func() bool {
		b.mu.Lock()
		defer b.mu.Unlock()
		return len(b.open) == 1
	}, 2*time.Second, 5*time.Millisecond)

	require.NoError(t, b.Resolve(1, interactionAnswer{Text: "set once"}))
	// resolving again reports settled rather than double-ending
	require.ErrorIs(t, b.Resolve(1, interactionAnswer{Text: "again"}), ErrInteractionSettled)
	// a stale id is unknown, never a silent no-op
	require.ErrorIs(t, b.Resolve(99, interactionAnswer{}), ErrInteractionUnknown)

	require.Len(t, log.endsSeen(), 1)
}

func TestBrokerLocalErrNoUIStaysOpen(t *testing.T) {
	t.Parallel()

	// serve mode's shape: a local surface exists but cannot ask
	local := &scriptedLocal{errs: []error{tui.ErrNoUI}, mode: tui.ModePlain}
	b, log := newBroker(local)
	resolved := make(chan error, 1)
	go func() {
		_, err := b.Select(t.Context(), "Proceed?", []tui.Option{{Label: "Yes"}})
		resolved <- err
	}()

	require.Eventually(t, func() bool {
		log.mu.Lock()
		defer log.mu.Unlock()
		return len(log.begins) == 1
	}, 2*time.Second, 5*time.Millisecond)

	// the local racer gave up; the dialog stays open for the remote answer
	require.NoError(t, b.Resolve(1, interactionAnswer{Index: 0}))
	require.NoError(t, <-resolved)
}

func TestBrokerDecisionRacesRemote(t *testing.T) {
	t.Parallel()

	local := &scriptedLocal{}
	b, log := newBroker(local)

	d, err := b.OpenDecision(tui.DecisionRequest{
		Prompt:  "Allow bash?",
		Context: "rm -rf /tmp/x",
		Options: []tui.Option{{Label: "Allow"}, {Label: "Deny"}},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return len(log.beginsSeen()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	begins := log.beginsSeen()
	assert.Equal(t, dialogDecision, begins[0].Kind)
	assert.Equal(t, "rm -rf /tmp/x", begins[0].Subject)

	go d.Resolve(0)
	idx, werr := d.Wait(t.Context())
	require.NoError(t, werr)
	assert.Equal(t, 0, idx)

	d.Close() // resolved already: a no-op

	require.Eventually(t, func() bool {
		return len(log.endsSeen()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	assert.True(t, log.endsSeen()[0].outcome.Answered)
}

func TestBrokerDecisionEscIsDenied(t *testing.T) {
	t.Parallel()

	b, _ := newBroker(nil)
	d, err := b.OpenDecision(tui.DecisionRequest{
		Prompt:  "Allow?",
		Options: []tui.Option{{Label: "Allow"}, {Label: "Deny"}},
	})
	require.NoError(t, err)

	settled := make(chan error, 1)
	go func() {
		settled <- b.Resolve(firstOpenID(b), interactionAnswer{Declined: true})
	}()
	require.NoError(t, <-settled)
	_, werr := d.Wait(t.Context())
	require.ErrorIs(t, werr, permit.ErrDenied)
}

// firstOpenID blocks until a dialog registers and returns its id.
func firstOpenID(b *interactionBroker) uint64 {
	for {
		b.mu.Lock()
		if len(b.open) > 0 {
			var id uint64
			for k := range b.open {
				id = k
			}
			b.mu.Unlock()
			return id
		}
		b.mu.Unlock()
		runtime.Gosched()
	}
}

func TestBrokerDecisionPlainModeRefuses(t *testing.T) {
	t.Parallel()

	local := &scriptedLocal{mode: tui.ModePlain}
	b, _ := newBroker(local)
	_, err := b.OpenDecision(tui.DecisionRequest{
		Prompt: "Allow?", Options: []tui.Option{{Label: "Allow"}},
	})
	assert.ErrorIs(t, err, tui.ErrNoUI)
}

// TestBrokerDecisionRootCancelSettles pins the teardown parenting: a decision
// dialog roots the app root, so a run with no local racer settles when the run
// quits instead of holding open past shutdown.
func TestBrokerDecisionRootCancelSettles(t *testing.T) {
	t.Parallel()

	root, cancel := context.WithCancel(t.Context())
	b, log := newBroker(nil) // serve shape: nothing local to ask
	b.root = root

	d, err := b.OpenDecision(tui.DecisionRequest{
		Prompt: "Allow?", Options: []tui.Option{{Label: "Allow"}, {Label: "Deny"}},
	})
	require.NoError(t, err)

	settled := make(chan error, 1)
	go func() {
		_, werr := d.Wait(t.Context())
		settled <- werr
	}()

	cancel() // teardown: the run quits with the dialog still open

	select {
	case werr := <-settled:
		require.ErrorIs(t, werr, permit.ErrDenied) // cancelled reads as denial, never a hang
	case <-time.After(5 * time.Second):
		t.Fatal("the open decision never settled after root cancel")
	}
	require.Eventually(t, func() bool { return len(log.endsSeen()) == 1 },
		2*time.Second, 5*time.Millisecond)
	assert.True(t, log.endsSeen()[0].outcome.Cancelled)
}

func TestBrokerDecisionNoOptionsWaitsErrNoUI(t *testing.T) {
	t.Parallel()

	b, _ := newBroker(nil)
	d, err := b.OpenDecision(tui.DecisionRequest{Prompt: "Allow?", Options: nil})
	require.NoError(t, err)
	_, werr := d.Wait(t.Context())
	assert.ErrorIs(t, werr, tui.ErrNoUI)
}

func TestBrokerCancelledContextSettles(t *testing.T) {
	t.Parallel()

	b, log := newBroker(nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = b.Input(ctx, "Name", "")
	}()

	require.Eventually(t, func() bool {
		return len(log.beginsSeen()) == 1
	}, 2*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	// every path ends: the cancel settles with no answer
	ends := log.endsSeen()
	require.Len(t, ends, 1)
	assert.False(t, ends[0].outcome.Answered)
	assert.True(t, ends[0].outcome.Cancelled)
}
