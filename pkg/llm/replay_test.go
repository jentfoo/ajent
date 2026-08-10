package llm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// capturedRequest records what an adapter actually sent.
type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

// sseServer replays a fixture as an event stream, flushing after every frame so
// the client sees them incrementally rather than as one buffered blob.
func sseServer(t *testing.T, fixture string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	return sseServerChunked(t, fixture, 0)
}

// sseServerChunked replays a fixture in chunks of n bytes, so a write boundary
// can land inside a JSON token and prove the reader reassembles before the
// decoder ever sees it. A zero n writes one frame at a time.
func sseServerChunked(t *testing.T, fixture string, n int) (*httptest.Server, *capturedRequest) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	require.NoError(t, err)

	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)

		for _, chunk := range splitStream(string(data), n) {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// splitStream slices a fixture into write units: whole frames when n is zero,
// otherwise fixed size chunks.
func splitStream(data string, n int) []string {
	if n <= 0 {
		frames := strings.SplitAfter(data, "\n\n")
		out := make([]string, 0, len(frames))
		for _, f := range frames {
			if f != "" {
				out = append(out, f)
			}
		}
		return out
	}
	var out []string
	for i := 0; i < len(data); i += n {
		out = append(out, data[i:min(i+n, len(data))])
	}
	return out
}

// jsonServer serves a fixture file as a JSON response and records the request.
func jsonServer(t *testing.T, fixture string) (*httptest.Server, *capturedRequest) {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", fixture))
	require.NoError(t, err)

	var got capturedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

// collect drains a stream, failing the test on a stream error.
func collect(t *testing.T, s Stream) []Event {
	t.Helper()

	t.Cleanup(func() { _ = s.Close() })
	var out []Event
	for ev, ok := s.Next(); ok; ev, ok = s.Next() {
		out = append(out, ev)
	}
	require.NoError(t, s.Err())
	return out
}

// eventKinds reduces events to their type names, for asserting stream shape.
func eventKinds(events []Event) []string {
	out := make([]string, len(events))
	for i, ev := range events {
		out[i] = ev.Type.String()
	}
	return out
}

// textOf joins every text delta.
func textOf(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == EventTextDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

// thinkingOf joins every thinking delta.
func thinkingOf(events []Event) string {
	var b strings.Builder
	for _, ev := range events {
		if ev.Type == EventThinkingDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}
