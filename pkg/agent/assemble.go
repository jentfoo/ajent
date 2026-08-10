package agent

import (
	"github.com/jentfoo/ajent/pkg/llm"
)

// assemble returns the message list for one request, system prompt excluded. It
// is a pure function of State plus the transform hook and never mutates State.
func assemble(s *State, t Transform) []llm.Message {
	messages := s.Messages
	if t != nil {
		messages = t(messages)
	}
	return messages
}
