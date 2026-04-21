// Package limiter defines the rate-limiter contract and supplies
// pluggable backends (in-memory, Redis). Callers depend only on the
// Limiter interface so backends can be swapped without touching HTTP
// handlers or business logic.
package limiter

import (
	"context"
	"time"
)

// Result is what a limiter returns from a single CheckAndRecord call.
type Result struct {
	Allowed         bool
	Remaining       int       // requests remaining in current window
	Limit           int       // configured cap
	RetryAfter      time.Duration
	ResetAt         time.Time // wall-clock time when oldest slot frees up
}

// UserStats is the per-user counters surfaced by GET /stats.
type UserStats struct {
	UserID             string `json:"user_id"`
	TotalRequests      int64  `json:"total_requests"`
	AllowedRequests    int64  `json:"allowed_requests"`
	RejectedRequests   int64  `json:"rejected_requests"`
	CurrentWindowCount int    `json:"current_window_count"`
	Limit              int    `json:"limit"`
	WindowSeconds      int    `json:"window_seconds"`
}

// Limiter is the abstraction implemented by every backend.
//
// CheckAndRecord MUST be atomic: between deciding whether the request
// fits and recording it, no other goroutine (or process, in the Redis
// case) may mutate the same user's window. This is what keeps the
// sliding-window count accurate under parallel calls.
type Limiter interface {
	CheckAndRecord(ctx context.Context, userID string) (Result, error)
	Stats(ctx context.Context, userID string) (UserStats, error)
	AllStats(ctx context.Context) ([]UserStats, error)
	Reset(ctx context.Context, userID string) error // empty userID resets all
	Close() error
	Limit() int
	Window() time.Duration
}
