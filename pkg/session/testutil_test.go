package session

import (
	"encoding/json"

	"github.com/jentfoo/ajent/pkg/llm"
)

// ids returns each entry's id in order.
func ids(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// msgData marshals a user text message payload for tests.
func msgData(text string) json.RawMessage {
	b, _ := json.Marshal(MessageData{Message: llm.Text(llm.RoleUser, text)})
	return b
}

// llmText builds a bare single-block message of the given role and text.
func llmText(text string) llm.Message {
	return llm.Text(llm.RoleUser, text)
}

// noticeData marshals a notice payload for tests.
func noticeData(msg string) json.RawMessage {
	b, _ := json.Marshal(NoticeData{Message: msg})
	return b
}
