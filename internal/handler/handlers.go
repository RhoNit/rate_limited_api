package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"go.uber.org/zap"

	"github.com/RhoNit/rate-limited-api/internal/config"
	"github.com/RhoNit/rate-limited-api/internal/limiter"
	"github.com/RhoNit/rate-limited-api/internal/model"
	"github.com/RhoNit/rate-limited-api/internal/queue"
)

type Handler struct {
	cfg *config.Config
	rl  limiter.Limiter
	q   *queue.Manager
	log *zap.Logger
}

func NewHandler(cfg *config.Config, rl limiter.Limiter, q *queue.Manager, log *zap.Logger) *Handler {
	return &Handler{cfg: cfg, rl: rl, q: q, log: log.Named("api")}
}

func (h *Handler) HandleRequest(c echo.Context) error {
	var in model.RequestIn
	if err := c.Bind(&in); err != nil {
		return c.JSON(http.StatusBadRequest, model.ErrorBody{Error: "invalid_json", Message: err.Error()})
	}
	in.UserID = strings.TrimSpace(in.UserID)
	if in.UserID == "" {
		return c.JSON(http.StatusBadRequest, model.ErrorBody{Error: "validation_error", Message: "user_id is required"})
	}
	if len(in.Payload) == 0 {
		return c.JSON(http.StatusBadRequest, model.ErrorBody{Error: "validation_error", Message: "payload is required"})
	}

	res, err := h.rl.CheckAndRecord(c.Request().Context(), in.UserID)
	if err != nil {
		h.log.Error("limiter error", zap.String("user_id", in.UserID), zap.Error(err))
		return c.JSON(http.StatusInternalServerError, model.ErrorBody{Error: "internal_error", Message: "rate limiter unavailable"})
	}

	c.Response().Header().Set("X-RateLimit-Limit", strconv.Itoa(res.Limit))
	c.Response().Header().Set("X-RateLimit-Remaining", strconv.Itoa(max(0, res.Remaining)))
	c.Response().Header().Set("X-RateLimit-Reset", epochStr(res.ResetAt))

	if res.Allowed {
		h.log.Info("allowed", zap.String("user_id", in.UserID), zap.Int("remaining", res.Remaining))
		return c.JSON(http.StatusOK, model.RequestOut{
			Status:    "accepted",
			UserID:    in.UserID,
			Remaining: res.Remaining,
			Limit:     res.Limit,
			ResetAt:   toEpoch(res.ResetAt),
		})
	}

	// Rate-limited: enqueue if queue is available, else 429.
	if h.q != nil {
		job := queue.Job{JobID: queue.NewJobID(), UserID: in.UserID, Payload: in.Payload}
		if err := h.q.Publish(c.Request().Context(), job, res.RetryAfter); err != nil {
			h.log.Error("queue publish failed", zap.String("user_id", in.UserID), zap.Error(err))
		} else {
			return c.JSON(http.StatusAccepted, model.QueuedOut{
				Status:            "queued",
				JobID:             job.JobID,
				UserID:            in.UserID,
				RetryAfterSeconds: res.RetryAfter.Seconds(),
			})
		}
	}

	h.log.Info("rate_limited", zap.String("user_id", in.UserID), zap.Duration("retry_after", res.RetryAfter))
	c.Response().Header().Set("Retry-After", strconv.Itoa(int(res.RetryAfter.Seconds())+1))
	return c.JSON(http.StatusTooManyRequests, model.ErrorBody{
		Error:             "rate_limit_exceeded",
		Message:           "limit is " + strconv.Itoa(res.Limit) + " per " + strconv.Itoa(int(h.cfg.RateWindow.Seconds())) + "s",
		RetryAfterSeconds: res.RetryAfter.Seconds(),
		ResetAt:           toEpoch(res.ResetAt),
	})
}

func (h *Handler) HandleStats(c echo.Context) error {
	uid := strings.TrimSpace(c.QueryParam("user_id"))
	ctx := c.Request().Context()

	var users []limiter.UserStats
	if uid != "" {
		st, err := h.rl.Stats(ctx, uid)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, model.ErrorBody{Error: "internal_error", Message: err.Error()})
		}
		users = []limiter.UserStats{st}
	} else {
		all, err := h.rl.AllStats(ctx)
		if err != nil {
			return c.JSON(http.StatusInternalServerError, model.ErrorBody{Error: "internal_error", Message: err.Error()})
		}
		users = all
	}

	out := model.StatsOut{
		Backend:       h.cfg.Backend,
		Limit:         h.cfg.RateLimit,
		WindowSeconds: int(h.cfg.RateWindow.Seconds()),
		Users:         users,
	}
	if h.q != nil {
		qs := h.q.Stats()
		out.Queue = &qs
	}
	return c.JSON(http.StatusOK, out)
}

func toEpoch(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }
func epochStr(t time.Time) string  { return strconv.FormatFloat(toEpoch(t), 'f', 3, 64) }
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
