package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestControlExecutorSerializesAndPreempts pins the lane: queued verbs run in
// order, an interrupt jumps ahead of everything queued, and a shut-down lane
// refuses legibly instead of hanging.
func TestControlExecutorSerializesAndPreempts(t *testing.T) {
	t.Parallel()

	e := newControlExecutor(t.Context())
	e.start()

	var order []int
	var mu sync.Mutex
	record := func(n int) func() {
		return func() {
			mu.Lock()
			order = append(order, n)
			mu.Unlock()
		}
	}
	blocked := make(chan struct{})
	release := make(chan struct{})

	require.True(t, e.run(func() { // occupies the lane
		close(blocked)
		<-release
	}))
	<-blocked
	require.True(t, e.run(record(1))) // queues behind
	require.True(t, e.run(record(2)))
	require.True(t, e.runPriority(record(9))) // preempts both

	close(release)
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 3
	}, 2*time.Second, 5*time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []int{9, 1, 2}, order)
}

func TestControlExecutorRefusesAfterShutdown(t *testing.T) {
	t.Parallel()

	root, cancel := context.WithCancel(t.Context())
	e := newControlExecutor(root)
	e.start()

	var ran atomic.Bool
	require.True(t, e.run(func() { ran.Store(true) }))
	cancel() // the app root cancels, like shutdown
	require.Eventually(t, func() bool { return ran.Load() }, 2*time.Second, 5*time.Millisecond)

	require.False(t, e.run(func() { ran.Store(false) })) // refused, not queued
	assert.True(t, ran.Load())
}
