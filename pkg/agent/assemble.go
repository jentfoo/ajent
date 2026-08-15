package agent

import (
	"github.com/jentfoo/ajent/pkg/llm"
)

// assemble returns the message list for one request, system prompt excluded. It
// is a pure function of State plus the ordered transform chain and never mutates
// State.
func assemble(s *State, ts []Transform) []llm.Message {
	messages := s.Messages
	for _, t := range ts {
		if t == nil {
			continue // nil entries are identity
		}
		messages = t(messages)
	}
	return messages
}
