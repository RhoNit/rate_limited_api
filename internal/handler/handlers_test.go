package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/RhoNit/rate-limited-api/internal/config"
	"github.com/RhoNit/rate-limited-api/internal/limiter"
	"github.com/RhoNit/rate-limited-api/internal/model"
)

// newTestServer mirrors the route registration in main.go so tests
// exercise the same handler wiring as production.
func newTestServer(t *testing.T, limit int, window time.Duration) *echo.Echo {
	t.Helper()
	cfg := &config.Config{
		HTTPAddr:   ":0",
		Backend:    "memory",
		RateLimit:  limit,
		RateWindow: window,
	}
	rl, err := limiter.NewMemory(limit, window)
	require.NoError(t, err)
	t.Cleanup(func() { _ = rl.Close() })

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	h := NewHandler(cfg, rl, nil, zap.NewNop())
	e.POST("/request", h.HandleRequest)
	e.GET("/stats", h.HandleStats)
	return e
}

func doPost(e *echo.Echo, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/request", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestPostRequest_AllowsThenBlocks(t *testing.T) {
	e := newTestServer(t, 3, time.Minute)

	for i := 0; i < 3; i++ {
		rec := doPost(e, `{"user_id":"alice","payload":{"x":1}}`)
		assert.Equal(t, http.StatusOK, rec.Code, "request %d should be allowed", i+1)
		assert.Equal(t, "3", rec.Header().Get("X-RateLimit-Limit"))
	}

	rec := doPost(e, `{"user_id":"alice","payload":{"x":1}}`)
	assert.Equal(t, http.StatusTooManyRequests, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "rate_limit_exceeded", body["error"])
}

func TestPostRequest_PerUserIsolated(t *testing.T) {
	e := newTestServer(t, 1, time.Minute)

	assert.Equal(t, http.StatusOK, doPost(e, `{"user_id":"alice","payload":1}`).Code)
	assert.Equal(t, http.StatusTooManyRequests, doPost(e, `{"user_id":"alice","payload":1}`).Code)
	assert.Equal(t, http.StatusOK, doPost(e, `{"user_id":"bob","payload":1}`).Code, "bob should have an independent quota")
}

func TestPostRequest_Validation(t *testing.T) {
	e := newTestServer(t, 5, time.Minute)

	cases := []struct {
		name string
		body string
	}{
		{"missing user_id", `{"payload":{}}`},
		{"blank user_id", `{"user_id":"   ","payload":{}}`},
		{"missing payload", `{"user_id":"x"}`},
		{"invalid json", `{"user_id":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, http.StatusBadRequest, doPost(e, tc.body).Code)
		})
	}
}

func TestStats_Aggregation(t *testing.T) {
	e := newTestServer(t, 2, time.Minute)
	doPost(e, `{"user_id":"alice","payload":{}}`)
	doPost(e, `{"user_id":"alice","payload":{}}`)
	doPost(e, `{"user_id":"alice","payload":{}}`) // rejected
	doPost(e, `{"user_id":"bob","payload":{}}`)

	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var out model.StatsOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, 2, out.Limit)

	byUser := map[string]limiter.UserStats{}
	for _, u := range out.Users {
		byUser[u.UserID] = u
	}
	assert.Equal(t, int64(2), byUser["alice"].AllowedRequests)
	assert.Equal(t, int64(1), byUser["alice"].RejectedRequests)
	assert.Equal(t, int64(1), byUser["bob"].AllowedRequests)
}

func TestStats_SingleUserFilter(t *testing.T) {
	e := newTestServer(t, 5, time.Minute)
	doPost(e, `{"user_id":"carol","payload":{}}`)

	req := httptest.NewRequest(http.MethodGet, "/stats?user_id=carol", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	var out model.StatsOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Users, 1)
	assert.Equal(t, "carol", out.Users[0].UserID)
}

// TestPostRequest_ConcurrentSafety fires 200 parallel requests for one user
// and asserts exactly `limit` succeed — verifies the mutex is effective.
func TestPostRequest_ConcurrentSafety(t *testing.T) {
	const limit, N = 5, 200
	e := newTestServer(t, limit, time.Minute)

	var wg sync.WaitGroup
	var allowed, rejected int64
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			switch doPost(e, `{"user_id":"rush","payload":{}}`).Code {
			case http.StatusOK:
				atomic.AddInt64(&allowed, 1)
			case http.StatusTooManyRequests:
				atomic.AddInt64(&rejected, 1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int64(limit), allowed)
	assert.Equal(t, int64(N-limit), rejected)
}
