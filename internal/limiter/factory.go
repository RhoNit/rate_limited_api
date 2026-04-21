package limiter

import (
	"context"
	"fmt"

	"github.com/RhoNit/rate-limited-api/internal/config"
)

// Build returns the Limiter selected by config.
func Build(ctx context.Context, cfg *config.Config) (Limiter, error) {
	switch cfg.Backend {
	case "memory":
		return NewMemory(cfg.RateLimit, cfg.RateWindow)
	case "mysql":
		return NewMySQL(ctx, cfg.MySQLDSN, cfg.RateLimit, cfg.RateWindow)
	default:
		return nil, fmt.Errorf("unknown backend %q (use memory|mysql)", cfg.Backend)
	}
}
