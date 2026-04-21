package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/RhoNit/rate-limited-api/internal/handler"
	"github.com/RhoNit/rate-limited-api/internal/config"
	"github.com/RhoNit/rate-limited-api/internal/limiter"
	"github.com/RhoNit/rate-limited-api/internal/queue"
)

func main() {
	// Build logger first so config.Load() can use zap.L().
	log := buildLogger()
	defer log.Sync() //nolint:errcheck

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config error", zap.Error(err))
	}
	log.Info("starting",
		zap.String("addr", cfg.HTTPAddr),
		zap.String("backend", cfg.Backend),
		zap.Int("limit", cfg.RateLimit),
		zap.Duration("window", cfg.RateWindow),
		zap.Bool("queue", cfg.QueueEnabled),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	rl, err := limiter.Build(ctx, cfg)
	if err != nil {
		log.Fatal("limiter error", zap.Error(err))
	}
	defer rl.Close() //nolint:errcheck

	var queueMgr *queue.Manager
	if cfg.QueueEnabled {
		queueMgr = queue.NewManager(cfg.AmqpURL, rl, log)
		if err := queueMgr.Start(ctx); err != nil {
			log.Fatal("queue error", zap.Error(err))
		}
		defer queueMgr.Close() //nolint:errcheck
	}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.Use(middleware.Recover())
	e.Use(middleware.Logger())

	h := handler.NewHandler(cfg, rl, queueMgr, log)
	e.POST("/request", h.HandleRequest)
	e.GET("/stats", h.HandleStats)

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: e,
	}

	go func() {
		log.Info("listening", zap.String("addr", cfg.HTTPAddr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown error", zap.Error(err))
	}
}

// buildLogger returns a development (coloured, human-readable) logger when
// LOG_DEVELOPMENT=true, otherwise a production JSON logger.
// It also registers itself as the zap global so zap.L() works everywhere.
func buildLogger() *zap.Logger {
	var cfg zap.Config
	if os.Getenv("LOG_DEVELOPMENT") == "true" {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		cfg = zap.NewProductionConfig()
	}
	log, err := cfg.Build()
	if err != nil {
		panic("build logger: " + err.Error())
	}
	zap.ReplaceGlobals(log)
	return log
}
