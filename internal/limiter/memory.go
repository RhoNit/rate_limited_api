package limiter

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// MemoryLimiter is a sliding-window-log limiter that lives entirely in
// process memory. Why sliding window log instead of a fixed counter:
//
//   - Fixed-window counters allow up to 2*N requests at the boundary
//     (N at the very end of one window, N at the start of the next).
//   - Sliding-window log evicts request timestamps older than the
//     window, so the cap is honoured at every moment in time.
//   - Per-user storage is bounded: at most `limit` timestamps are
//     retained, so memory stays small even for very chatty users.
//
// Concurrency model
//
//   - We hold a per-user mutex around the read-evict-decide-record
//     critical section. This serialises a single user's traffic but
//     allows different users to be processed fully in parallel.
//   - The user registry itself uses a single RWMutex; the fast path
//     grabs an RLock so concurrent users with existing state never
//     contend on the registry.
//
// Limitations (documented in README): in-process state means scaling
// horizontally requires sticky routing or switching to the Redis
// backend; idle users are not garbage-collected automatically (a
// background sweeper would address this).
type MemoryLimiter struct {
	limit  int
	window time.Duration

	mu    sync.RWMutex
	users map[string]*userState
}

type userState struct {
	mu       sync.Mutex
	stamps   *list.List // values are time.Time, oldest at front
	total    int64
	allowed  int64
	rejected int64
}

// NewMemory builds a MemoryLimiter. Returns an error on invalid args
// rather than panicking so config errors surface from main().
func NewMemory(limit int, window time.Duration) (*MemoryLimiter, error) {
	if limit <= 0 {
		return nil, errors.New("limit must be positive")
	}
	if window <= 0 {
		return nil, errors.New("window must be positive")
	}
	return &MemoryLimiter{
		limit:  limit,
		window: window,
		users:  make(map[string]*userState),
	}, nil
}

func (m *MemoryLimiter) Limit() int { 
	return m.limit 
}
func (m *MemoryLimiter) Window() time.Duration { 
	return m.window 
}
func (m *MemoryLimiter) Close() error { 
	return nil 
}

// getOrCreate returns the per-user state, creating it on first sight.
// Hot path is RLock-only; only the first request for a user takes the
// write lock briefly.
func (m *MemoryLimiter) getOrCreate(userID string) *userState {
	m.mu.RLock()
	st, ok := m.users[userID]
	m.mu.RUnlock()
	if ok {
		return st
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok = m.users[userID]; ok {
		return st
	}
	st = &userState{stamps: list.New()}
	m.users[userID] = st
	return st
}

// evict drops timestamps that fell out of the window.
func evict(l *list.List, cutoff time.Time) {
	for {
		front := l.Front()
		if front == nil {
			return
		}
		t := front.Value.(time.Time)
		if t.After(cutoff) {
			return
		}
		l.Remove(front)
	}
}

// CheckAndRecord is the atomic decision + record step. The whole
// critical section is held under the per-user mutex.
func (m *MemoryLimiter) CheckAndRecord(_ context.Context, userID string) (Result, error) {
	st := m.getOrCreate(userID)

	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-m.window)
	evict(st.stamps, cutoff)

	st.total++

	if st.stamps.Len() >= m.limit {
		st.rejected++
		oldest := st.stamps.Front().Value.(time.Time)
		retry := oldest.Add(m.window).Sub(now)
		if retry < 0 {
			retry = 0
		}
		return Result{
			Allowed:    false,
			Remaining:  0,
			Limit:      m.limit,
			RetryAfter: retry,
			ResetAt:    now.Add(retry),
		}, nil
	}

	st.stamps.PushBack(now)
	st.allowed++
	oldest := st.stamps.Front().Value.(time.Time)
	return Result{
		Allowed:    true,
		Remaining:  m.limit - st.stamps.Len(),
		Limit:      m.limit,
		RetryAfter: 0,
		ResetAt:    oldest.Add(m.window),
	}, nil
}

func (m *MemoryLimiter) Stats(_ context.Context, userID string) (UserStats, error) {
	m.mu.RLock()
	st, ok := m.users[userID]
	m.mu.RUnlock()
	if !ok {
		return UserStats{
			UserID:        userID,
			Limit:         m.limit,
			WindowSeconds: int(m.window.Seconds()),
		}, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	evict(st.stamps, time.Now().Add(-m.window))
	return UserStats{
		UserID:             userID,
		TotalRequests:      st.total,
		AllowedRequests:    st.allowed,
		RejectedRequests:   st.rejected,
		CurrentWindowCount: st.stamps.Len(),
		Limit:              m.limit,
		WindowSeconds:      int(m.window.Seconds()),
	}, nil
}

func (m *MemoryLimiter) AllStats(ctx context.Context) ([]UserStats, error) {
	m.mu.RLock()
	ids := make([]string, 0, len(m.users))
	for id := range m.users {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	out := make([]UserStats, 0, len(ids))
	for _, id := range ids {
		s, err := m.Stats(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *MemoryLimiter) Reset(_ context.Context, userID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if userID == "" {
		m.users = make(map[string]*userState)
		return nil
	}
	delete(m.users, userID)
	return nil
}
