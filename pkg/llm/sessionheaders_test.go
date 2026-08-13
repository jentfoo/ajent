package llm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionAffinityHeaders(t *testing.T) {
	t.Parallel()

	t.Run("anthropic_adds_x_session_affinity", func(t *testing.T) {
		srv, got := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)
		m := anthropicModel(func(c *Capabilities) { c.SessionAffinity = true })

		s, err := p.Stream(t.Context(), Request{Model: m, SessionID: "sess-1"})
		require.NoError(t, err)
		collect(t, s)

		assert.Equal(t, "sess-1", got.Header.Get("x-session-affinity"))
	})

	t.Run("anthropic_no_beta_still_affixed", func(t *testing.T) {
		srv, got := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)
		// no reasoning means no interleaved beta header, so the affinity header must
		// still be applied on the shared-headers path
		m := anthropicModel(func(c *Capabilities) { c.SessionAffinity = true; c.Reasoning = false })

		s, err := p.Stream(t.Context(), Request{Model: m, SessionID: "sess-1"})
		require.NoError(t, err)
		collect(t, s)

		assert.Equal(t, "sess-1", got.Header.Get("x-session-affinity"))
	})

	t.Run("anthropic_omits_when_not_gated", func(t *testing.T) {
		srv, got := sseServer(t, "anthropic/text.sse")
		p := newAnthropicTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: anthropicModel(nil), SessionID: "sess-1"})
		require.NoError(t, err)
		collect(t, s)

		assert.Empty(t, got.Header.Get("x-session-affinity"))
	})

	t.Run("responses_openai_format", func(t *testing.T) {
		srv, got := sseServer(t, "openai/text.sse")
		p := newResponsesTestProvider(t, srv.URL)
		m := responsesModel(func(c *Capabilities) { c.SessionAffinity = true })

		s, err := p.Stream(t.Context(), Request{Model: m, SessionID: "sess-1"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		for ev, ok := s.Next(); ok; _, ok = s.Next() {
			_ = ev
		}

		assert.Equal(t, "sess-1", got.Header.Get("session_id"))
		assert.Equal(t, "sess-1", got.Header.Get("x-client-request-id"))
	})

	t.Run("compat_openrouter_format", func(t *testing.T) {
		srv, got := sseServer(t, "compat/text.sse")
		p := newCompatTestProvider(t, srv.URL)
		m := compatModel(func(c *Capabilities) { c.SessionAffinity = true; c.SessionAffinityFormat = openRouterAffinityFormat })

		s, err := p.Stream(t.Context(), Request{Model: m, SessionID: "sess-1"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		for ev, ok := s.Next(); ok; _, ok = s.Next() {
			_ = ev
		}

		assert.Equal(t, "sess-1", got.Header.Get("x-session-id"))
	})

	t.Run("model_headers_not_mutated", func(t *testing.T) {
		m := responsesModel(func(c *Capabilities) { c.SessionAffinity = true })
		m.Headers = map[string]string{"X-Keep": "yes"}
		srv, got := sseServer(t, "openai/text.sse")
		p := newResponsesTestProvider(t, srv.URL)

		s, err := p.Stream(t.Context(), Request{Model: m, SessionID: "sess-1"})
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })
		for ev, ok := s.Next(); ok; _, ok = s.Next() {
			_ = ev
		}

		assert.Equal(t, "yes", got.Header.Get("X-Keep"))
		assert.NotContains(t, m.Headers, "session_id")
	})
}
