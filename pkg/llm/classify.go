package llm

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jentfoo/ajent/pkg/httputil"
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
	return slices.ContainsFunc(overflowPhrases[flavor], func(phrase string) bool {
		return strings.Contains(lower, phrase)
	})
}

// frameStatuses maps in-band error codes onto the status the same condition
// carries pre-stream, so a mid-stream frame retries like the response it
// mirrors. Anthropic type names plus the two openai responses codes.
var frameStatuses = map[string]int{
	"invalid_request_error": http.StatusBadRequest,
	"authentication_error":  http.StatusUnauthorized,
	"billing_error":         http.StatusBadRequest,
	"permission_error":      http.StatusForbidden,
	"not_found_error":       http.StatusNotFound,
	"request_too_large":     http.StatusRequestEntityTooLarge,
	"rate_limit_error":      http.StatusTooManyRequests,
	"timeout_error":         http.StatusRequestTimeout,
	"api_error":             http.StatusInternalServerError,
	"overloaded_error":      httputil.StatusOverloaded,
	"server_error":          http.StatusInternalServerError,
	"rate_limit_exceeded":   http.StatusTooManyRequests,
}

// frameStatus parses a mid-stream frame's code into an HTTP status. Vendor app
// codes outside 4xx/5xx do not parse, keeping the frame unrecoverable.
func frameStatus(code string) (int, bool) {
	if status, ok := frameStatuses[code]; ok {
		return status, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(code))
	if err != nil || n < 400 || n > 599 {
		return 0, false
	}
	return n, true
}

// frameAPIError builds a mid-stream error frame's error, verdicted like a
// pre-stream response when the code parses to a status.
func frameAPIError(provider, code, message string, flavor Flavor) *APIError {
	e := &APIError{Provider: provider, Code: code, Message: message}
	status, ok := frameStatus(code)
	if !ok {
		return e
	}
	e.Status = status
	if isOverflowStatus(status, flavor) && matchesOverflow(message+" "+code, flavor) {
		return e.Overflow()
	}
	e.Retryable = httputil.ShouldRetryStatus(status, false)
	return e
}
