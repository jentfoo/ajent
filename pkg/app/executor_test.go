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

func TestControlExecutorDrainsPastCancel(t *testing.T) {
	t.Parallel()

	// enqueues racing the cancellation must never strand a verb: everything
	// accepted runs once, so quiesce's wait ends without hitting its bound.
	for range 50 {
		root, cancel := context.WithCancel(t.Context())
		e := newControlExecutor(root)
		e.start()

		var ran, refused atomic.Int64
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if e.run(func() { ran.Add(1) }) {
					return
				}
				refused.Add(1)
			}()
		}
		cancel()
		wg.Wait()

		assert.False(t, e.quiesce(2*time.Second))
		assert.Equal(t, 4-refused.Load(), ran.Load())
	}
}

func TestControlExecutorQuiesce(t *testing.T) {
	t.Parallel()

	t.Run("waits_for_held_work", func(t *testing.T) {
		e := newControlExecutor(t.Context())
		e.start()
		blocked := make(chan struct{})
		release := make(chan struct{})
		require.True(t, e.run(func() {
			close(blocked)
			<-release
		}))
		<-blocked

		var done atomic.Bool
		go func() {
			e.quiesce(time.Hour)
			done.Store(true)
		}()
		require.Never(t, done.Load, 50*time.Millisecond, 5*time.Millisecond)

		close(release)
		require.Eventually(t, done.Load, 2*time.Second, 5*time.Millisecond)
	})

	t.Run("refuses_new_work", func(t *testing.T) {
		e := newControlExecutor(t.Context())
		e.start()

		require.False(t, e.quiesce(time.Hour))
		require.False(t, e.run(func() {})) // armed: never queued
	})

	t.Run("bounded_on_stuck_verb", func(t *testing.T) {
		e := newControlExecutor(t.Context())
		e.start()
		blocked := make(chan struct{})
		require.True(t, e.run(func() { <-blocked }))
		t.Cleanup(func() { close(blocked) })

		assert.True(t, e.quiesce(20*time.Millisecond))
	})
}
