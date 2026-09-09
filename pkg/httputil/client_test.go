package httputil

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dur returns a pointer to d.
func dur(d time.Duration) *time.Duration { return &d }

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("no_client_timeout", func(t *testing.T) {
		assert.Zero(t, New(Options{}).Timeout)
	})
	t.Run("shared_for_equal_bounds", func(t *testing.T) {
		defaults, alsoDefaults := New(Options{}), New(Options{})
		assert.Same(t, defaults, alsoDefaults)

		bounded := New(Options{Timeouts: Timeouts{TLS: dur(time.Minute)}})
		alsoBounded := New(Options{Timeouts: Timeouts{TLS: dur(time.Minute)}})
		assert.Same(t, bounded, alsoBounded)
	})
	t.Run("distinct_for_differing_bounds", func(t *testing.T) {
		assert.NotSame(t, New(Options{}), New(Options{Timeouts: Timeouts{TLS: dur(time.Second)}}))
	})
	t.Run("bounds_reach_the_transport", func(t *testing.T) {
		tr, ok := New(Options{Timeouts: Timeouts{
			Connect: dur(11 * time.Second),
			TLS:     dur(12 * time.Second),
			Header:  dur(34 * time.Second),
		}}).Transport.(*http.Transport)
		require.True(t, ok)

		assert.Equal(t, 12*time.Second, tr.TLSHandshakeTimeout)
		assert.Equal(t, 34*time.Second, tr.ResponseHeaderTimeout)
	})
	t.Run("explicit_zero_disables_bound", func(t *testing.T) {
		tr, ok := New(Options{Timeouts: Timeouts{Header: dur(0)}}).Transport.(*http.Transport)
		require.True(t, ok)

		assert.Zero(t, tr.ResponseHeaderTimeout) // a lm-studio JIT load holds headers for minutes
		assert.Equal(t, defaultTLSTimeout, tr.TLSHandshakeTimeout)
	})
	t.Run("proxy_and_pool_bounds_set", func(t *testing.T) {
		tr, ok := New(Options{}).Transport.(*http.Transport)
		require.True(t, ok)

		assert.NotNil(t, tr.Proxy)
		assert.Equal(t, pooledConnTimeout, tr.IdleConnTimeout)
		assert.Equal(t, defaultMaxIdleConns, tr.MaxIdleConns)
		assert.Equal(t, defaultMaxIdleConnsPerHost, tr.MaxIdleConnsPerHost)
	})
	t.Run("transport_option_bypasses_cache", func(t *testing.T) {
		tr := &http.Transport{}
		c := New(Options{Transport: tr})
		assert.Same(t, tr, c.Transport)
		assert.NotSame(t, c, New(Options{Transport: tr}))
	})
}
