package llm

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// ErrScriptExhausted is returned when a ScriptedProvider runs out of turns.
var ErrScriptExhausted = errors.New("llm: scripted provider has no turns left")

// ScriptedTurn is one canned response.
type ScriptedTurn struct {
	Events []Event
	Err    error // returned from Stream when set, before any event
}

// ScriptedProvider replays canned turns in order and records the requests it
// was given. Use it wherever a real provider would be, in tests and fakes.
type ScriptedProvider struct {
	ProviderName string
	Turns        []ScriptedTurn

	mu       sync.Mutex
	pos      int
	requests []Request
}

// Name returns the configured provider name, or "scripted".
func (p *ScriptedProvider) Name() string {
	if p.ProviderName == "" {
		return "scripted"
	}
	return p.ProviderName
}

// Stream records req and replays the next scripted turn.
func (p *ScriptedProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.requests = append(p.requests, req)
	if p.pos >= len(p.Turns) {
		return nil, ErrScriptExhausted
	}
	turn := p.Turns[p.pos]
	p.pos++
	if turn.Err != nil {
		return nil, turn.Err
	}
	return &SliceStream{Events: turn.Events}, nil
}

// Requests returns the requests seen so far, in order.
func (p *ScriptedProvider) Requests() []Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.requests)
}
