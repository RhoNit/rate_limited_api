package limiter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryLimiter_AllowsUpToLimit(t *testing.T) {
	rl, err := NewMemory(5, time.Minute)
	require.NoError(t, err)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		r, err := rl.CheckAndRecord(ctx, "u")
		require.NoError(t, err)
		assert.True(t, r.Allowed, "request %d should be allowed", i+1)
		assert.Equal(t, 4-i, r.Remaining, "remaining after request %d", i+1)
	}

	r, _ := rl.CheckAndRecord(ctx, "u")
	assert.False(t, r.Allowed, "6th request should be rejected")
	assert.Greater(t, r.RetryAfter, time.Duration(0), "RetryAfter should be positive")
}

func TestMemoryLimiter_PerUserIsolation(t *testing.T) {
	rl, _ := NewMemory(1, time.Minute)
	ctx := context.Background()

	r, _ := rl.CheckAndRecord(ctx, "alice")
	assert.True(t, r.Allowed, "alice 1st request should pass")

	r, _ = rl.CheckAndRecord(ctx, "alice")
	assert.False(t, r.Allowed, "alice 2nd request should be rejected")

	r, _ = rl.CheckAndRecord(ctx, "bob")
	assert.True(t, r.Allowed, "bob should be unaffected by alice's limit")
}

func TestMemoryLimiter_SlidingWindowEviction(t *testing.T) {
	rl, _ := NewMemory(2, 200*time.Millisecond)
	ctx := context.Background()

	rl.CheckAndRecord(ctx, "u")
	rl.CheckAndRecord(ctx, "u")

	r, _ := rl.CheckAndRecord(ctx, "u")
	assert.False(t, r.Allowed, "3rd request within window should be rejected")

	time.Sleep(220 * time.Millisecond)

	r, _ = rl.CheckAndRecord(ctx, "u")
	assert.True(t, r.Allowed, "request after window expiry should be allowed")
}

func TestMemoryLimiter_ConstructionErrors(t *testing.T) {
	_, err := NewMemory(0, time.Minute)
	assert.Error(t, err, "limit=0 should return an error")

	_, err = NewMemory(5, 0)
	assert.Error(t, err, "window=0 should return an error")
}

func TestMemoryLimiter_Stats(t *testing.T) {
	rl, _ := NewMemory(2, time.Minute)
	ctx := context.Background()

	rl.CheckAndRecord(ctx, "u")
	rl.CheckAndRecord(ctx, "u")
	rl.CheckAndRecord(ctx, "u") // rejected
	rl.CheckAndRecord(ctx, "u") // rejected

	s, err := rl.Stats(ctx, "u")
	require.NoError(t, err)
	assert.Equal(t, int64(4), s.TotalRequests)
	assert.Equal(t, int64(2), s.AllowedRequests)
	assert.Equal(t, int64(2), s.RejectedRequests)
	assert.Equal(t, 2, s.CurrentWindowCount)
}

func TestMemoryLimiter_StatsUnknownUser(t *testing.T) {
	rl, _ := NewMemory(5, time.Minute)
	s, err := rl.Stats(context.Background(), "ghost")
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.TotalRequests)
	assert.Equal(t, 5, s.Limit)
}

func TestMemoryLimiter_Reset(t *testing.T) {
	rl, _ := NewMemory(1, time.Minute)
	ctx := context.Background()

	rl.CheckAndRecord(ctx, "a")
	rl.CheckAndRecord(ctx, "b")

	rl.Reset(ctx, "a")
	r, _ := rl.CheckAndRecord(ctx, "a")
	assert.True(t, r.Allowed, "a should pass after per-user reset")

	r, _ = rl.CheckAndRecord(ctx, "b")
	assert.False(t, r.Allowed, "b should still be capped")

	rl.Reset(ctx, "")
	r, _ = rl.CheckAndRecord(ctx, "b")
	assert.True(t, r.Allowed, "b should pass after global reset")
}

// TestMemoryLimiter_ConcurrentSingleUser fires 1000 goroutines against one
// user and asserts exactly `limit` are allowed — the critical correctness
// test for the per-user mutex.
func TestMemoryLimiter_ConcurrentSingleUser(t *testing.T) {
	const limit, N = 5, 1000
	rl, _ := NewMemory(limit, time.Minute)
	ctx := context.Background()

	var wg sync.WaitGroup
	var allowed, rejected int64
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			if r, _ := rl.CheckAndRecord(ctx, "rush"); r.Allowed {
				atomic.AddInt64(&allowed, 1)
			} else {
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(limit), allowed)
	assert.Equal(t, int64(N-limit), rejected)

	s, _ := rl.Stats(ctx, "rush")
	assert.Equal(t, int64(limit), s.AllowedRequests)
	assert.Equal(t, int64(N-limit), s.RejectedRequests)
}

// TestMemoryLimiter_ConcurrentManyUsers verifies per-user isolation holds
// under parallelism: 20 users × 50 requests each → exactly 20×5 allowed.
func TestMemoryLimiter_ConcurrentManyUsers(t *testing.T) {
	const limit, users, perUser = 5, 20, 50
	rl, _ := NewMemory(limit, time.Minute)
	ctx := context.Background()

	var wg sync.WaitGroup
	var allowed int64
	for u := 0; u < users; u++ {
		uid := fmt.Sprintf("user-%d", u)
		for i := 0; i < perUser; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r, _ := rl.CheckAndRecord(ctx, uid); r.Allowed {
					atomic.AddInt64(&allowed, 1)
				}
			}()
		}
	}
	wg.Wait()

	assert.Equal(t, int64(users*limit), allowed)
}
