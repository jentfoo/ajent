package agent

import (
	"context"
	"sync"

	"github.com/jentfoo/ajent/pkg/llm"
)

// hangStream is a Stream that delivers its first event immediately then blocks
// until Close, so an interrupt can land mid-stream. SliceStream ignores ctx and
// never blocks, which makes it useless for cancellation tests.
type hangStream struct {
	events []llm.Event

	mu   sync.Mutex
	pos  int
	done chan struct{}
}

func (s *hangStream) Next() (llm.Event, bool) {
	s.mu.Lock()
	if s.pos >= len(s.events) {
		s.mu.Unlock()
		return llm.Event{}, false
	}
	i := s.pos
	s.pos++
	blocked := i > 0 // deliver event zero immediately, hold the rest for Close
	ev := s.events[i]
	done := s.done
	s.mu.Unlock()

	if blocked {
		<-done
		return llm.Event{}, false
	}
	return ev, true
}

func (s *hangStream) Err() error { return nil }

// Close unblocks a pending Next and marks the stream done.
func (s *hangStream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done != nil {
		close(s.done)
		s.done = nil
	}
	return nil
}

// hangProvider serves one turn through a hangStream, for mid-stream interrupts.
type hangProvider struct {
	turn []llm.Event

	mu  sync.Mutex
	cur *hangStream
}

func (p *hangProvider) Name() string { return "hang" }

// Stream returns the single hanging stream and records it as current.
func (p *hangProvider) Stream(_ context.Context, _ llm.Request) (llm.Stream, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := &hangStream{events: p.turn, done: make(chan struct{})}
	p.cur = s
	return s, nil
}

// current returns the stream last served, or nil.
func (p *hangProvider) current() *hangStream {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}
