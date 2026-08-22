package llm

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrContextOverflow is the normalized "too many input tokens" signal every
	// adapter maps its vendor specific form to.
	ErrContextOverflow = errors.New("llm: context overflow")
	// ErrStreamAborted is returned when a stream failed after emitting content,
	// which is never retried because that would duplicate deltas.
	ErrStreamAborted = errors.New("llm: stream aborted after partial content")
	// ErrIdleTimeout is returned when a response stalled past the idle timeout.
	ErrIdleTimeout = errors.New("llm: stream idle timeout")
	// ErrMalformedToolArgs is reported on a tool call whose accumulated
	// arguments are not valid JSON. It fails the call, not the turn.
	ErrMalformedToolArgs = errors.New("llm: malformed tool arguments")
	// ErrUnknownModel is returned when no model matches a name.
	ErrUnknownModel = errors.New("llm: unknown model")
	// ErrNoTokenizer is returned by CountTokens when the provider has no exact
	// counting endpoint for the model.
	ErrNoTokenizer = errors.New("llm: no tokenizer endpoint")
)

// ErrNoAPIKey names the environment variable that would supply a missing key.
type ErrNoAPIKey struct {
	Provider string
	EnvVar   string
}

func (e *ErrNoAPIKey) Error() string {
	if e.EnvVar == "" {
		return "llm: no api key for " + e.Provider
	}
	return "llm: no api key for " + e.Provider + ", set " + e.EnvVar
}

// ErrAmbiguousModel lists the models a name matched.
type ErrAmbiguousModel struct {
	Name       string
	Candidates []string
}

func (e *ErrAmbiguousModel) Error() string {
	return "llm: " + strconv.Quote(e.Name) + " matches " + strings.Join(e.Candidates, ", ")
}

// APIError is a provider error response, with the vendor detail preserved.
type APIError struct {
	Provider   string
	Status     int
	Code       string
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Body       []byte // truncated and scrubbed, for the debug log
	// Hint names the models.json setting that would fix the request, for the
	// rejections a user cannot diagnose from the provider's own message.
	Hint string

	overflow bool // classified once, by the adapter that knows the dialect
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("llm: ")
	b.WriteString(e.Provider)
	if e.Status != 0 {
		b.WriteString(" http ")
		b.WriteString(strconv.Itoa(e.Status))
	}
	if e.Code != "" {
		b.WriteString(" ")
		b.WriteString(e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Hint != "" {
		b.WriteString(" (")
		b.WriteString(e.Hint)
		b.WriteString(")")
	}
	return b.String()
}

// Unwrap reports context overflow so callers can match it with errors.Is.
func (e *APIError) Unwrap() error {
	if e.overflow {
		return ErrContextOverflow
	}
	return nil
}

// Overflow marks the error as a context overflow, so errors.Is against
// ErrContextOverflow matches. Adapters call it during classification.
func (e *APIError) Overflow() *APIError {
	e.overflow = true
	e.Retryable = false
	return e
}

// IsOverflow reports whether err is or wraps a context overflow.
func IsOverflow(err error) bool { return errors.Is(err, ErrContextOverflow) }

// IsRetryable reports whether err is a provider error worth retrying.
func IsRetryable(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Retryable
}
