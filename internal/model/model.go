// Package model contains the request/response types for the HTTP API.
package model

import (
	"encoding/json"

	"github.com/RhoNit/rate-limited-api/internal/limiter"
	"github.com/RhoNit/rate-limited-api/internal/queue"
)

type RequestIn struct {
	UserID  string          `json:"user_id"`
	Payload json.RawMessage `json:"payload"`
}

type RequestOut struct {
	Status    string  `json:"status"`
	UserID    string  `json:"user_id"`
	Remaining int     `json:"remaining"`
	Limit     int     `json:"limit"`
	ResetAt   float64 `json:"reset_at_epoch"`
}

type QueuedOut struct {
	Status            string  `json:"status"`
	JobID             string  `json:"job_id"`
	UserID            string  `json:"user_id"`
	RetryAfterSeconds float64 `json:"retry_after_seconds"`
}

type ErrorBody struct {
	Error             string  `json:"error"`
	Message           string  `json:"message"`
	RetryAfterSeconds float64 `json:"retry_after_seconds,omitempty"`
	ResetAt           float64 `json:"reset_at_epoch,omitempty"`
}

type StatsOut struct {
	Backend       string              `json:"backend"`
	Limit         int                 `json:"limit"`
	WindowSeconds int                 `json:"window_seconds"`
	Users         []limiter.UserStats `json:"users"`
	Queue         *queue.Stats        `json:"queue,omitempty"`
}
