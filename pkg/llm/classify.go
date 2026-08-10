package llm

import (
	"encoding/json"
	"net/http"
	"strings"
)

// overflowPhrases are the substrings each vendor uses to say the input did not
// fit. Matching prose is fragile, so callers pair it with the token estimate
// check in classifyOverflow.
var overflowPhrases = map[Flavor][]string{
	FlavorAnthropic:  {"prompt is too long", "exceed context limit", "exceeds the maximum"},
	FlavorOpenAI:     {"maximum context length", "context_length_exceeded", "reduce the length"},
	FlavorOpenRouter: {"maximum context length", "context_length_exceeded", "context length"},
	FlavorLlamaCpp:   {"n_ctx", "exceeds the available context size", "context shift is disabled"},
	FlavorLMStudio:   {"context length", "token limit", "exceeds the context"},
	FlavorGeneric:    {"context length", "context_length_exceeded"},
}

// compatClassifier returns the error classifier for a chat-completions flavor.
func compatClassifier(provider string, flavor Flavor) func(int, []byte) error {
	return func(status int, body []byte) error {
		var wire compatErrBody
		_ = json.Unmarshal(body, &wire)

		msg := strings.TrimSpace(string(body))
		var code string
		if wire.Error != nil {
			if wire.Error.Message != "" {
				msg = wire.Error.Message
			}
			code = wire.Error.codeString()
			if len(wire.Error.Metadata) > 0 {
				msg += " " + string(wire.Error.Metadata) // openrouter hides the upstream text here
			}
		}

		err := &APIError{
			Provider: provider, Status: status, Code: code, Message: msg, Body: body,
			Retryable: shouldRetryStatus(status, false),
		}
		if isOverflowStatus(status, flavor) && matchesOverflow(msg+" "+code, flavor) {
			return err.Overflow()
		}
		return err
	}
}

// isOverflowStatus reports whether a status can carry an overflow. llama.cpp
// reports it as a 500 rather than a client error.
func isOverflowStatus(status int, flavor Flavor) bool {
	if status == http.StatusBadRequest {
		return true
	}
	return flavor == FlavorLlamaCpp && status == http.StatusInternalServerError
}

// matchesOverflow reports whether text carries a vendor's overflow wording.
func matchesOverflow(text string, flavor Flavor) bool {
	lower := strings.ToLower(text)
	for _, phrase := range overflowPhrases[flavor] {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
