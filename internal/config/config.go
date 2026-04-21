// Package config loads runtime configuration using spf13/viper.
//
// Priority (highest → lowest):
//  1. Environment variables
//  2. config.yml  (searched in ".", then "/app")
//  3. Built-in defaults
package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
)

type Config struct {
	HTTPAddr     string
	Backend      string        // "memory" | "mysql"
	RateLimit    int           // max requests per window per user
	RateWindow   time.Duration // sliding-window duration
	MySQLDSN     string
	AmqpURL      string
	QueueEnabled bool
}

func Load() (*Config, error) {
	v := viper.New()
	setDefaults(v)
	bindEnvVars(v)
	readConfigFile(v)
	return build(v)
}

func setDefaults(v *viper.Viper) {
	v.SetDefault("http.addr", ":8000")
	v.SetDefault("rate_limit.backend", "memory")
	v.SetDefault("rate_limit.max_requests", 5)
	v.SetDefault("rate_limit.window_seconds", 60)
	v.SetDefault("mysql.dsn", "root:secret@tcp(localhost:3306)/ratelimit")
	v.SetDefault("queue.enabled", false)
	v.SetDefault("queue.amqp_url", "amqp://guest:guest@localhost:5672/")
}

// bindEnvVars maps env var names onto viper keys.
//
//	Viper key                     ← Env var
//	http.addr                     ← HTTP_ADDR
//	rate_limit.backend            ← RATE_LIMIT_BACKEND
//	rate_limit.max_requests       ← RATE_LIMIT_MAX_REQUESTS
//	rate_limit.window_seconds     ← RATE_LIMIT_WINDOW_SECONDS
//	mysql.dsn                     ← MYSQL_DSN
//	queue.enabled                 ← QUEUE_ENABLED
//	queue.amqp_url                ← AMQP_URL
func bindEnvVars(v *viper.Viper) {
	pairs := [][2]string{
		{"http.addr", "HTTP_ADDR"},
		{"rate_limit.backend", "RATE_LIMIT_BACKEND"},
		{"rate_limit.max_requests", "RATE_LIMIT_MAX_REQUESTS"},
		{"rate_limit.window_seconds", "RATE_LIMIT_WINDOW_SECONDS"},
		{"mysql.dsn", "MYSQL_DSN"},
		{"queue.enabled", "QUEUE_ENABLED"},
		{"queue.amqp_url", "AMQP_URL"},
	}
	for _, p := range pairs {
		if err := v.BindEnv(p[0], p[1]); err != nil {
			panic(fmt.Sprintf("config: BindEnv(%q): %v", p[0], err))
		}
	}
}

func readConfigFile(v *viper.Viper) {
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	v.AddConfigPath("/app")

	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) {
			zap.L().Warn("error reading config file", zap.Error(err))
		}
		return
	}
	zap.L().Info("config loaded", zap.String("file", v.ConfigFileUsed()))
}

func build(v *viper.Viper) (*Config, error) {
	limit := v.GetInt("rate_limit.max_requests")
	if limit <= 0 {
		return nil, fmt.Errorf("rate_limit.max_requests must be > 0")
	}

	windowSec := v.GetInt("rate_limit.window_seconds")
	if windowSec <= 0 {
		return nil, fmt.Errorf("rate_limit.window_seconds must be > 0")
	}

	backend := v.GetString("rate_limit.backend")
	if backend != "memory" && backend != "mysql" {
		return nil, fmt.Errorf("rate_limit.backend %q is invalid (use memory|mysql)", backend)
	}

	dsn := v.GetString("mysql.dsn")
	if backend == "mysql" && !strings.Contains(dsn, "parseTime") {
		if strings.Contains(dsn, "?") {
			dsn += "&parseTime=true&loc=UTC"
		} else {
			dsn += "?parseTime=true&loc=UTC"
		}
	}

	return &Config{
		HTTPAddr:     v.GetString("http.addr"),
		Backend:      backend,
		RateLimit:    limit,
		RateWindow:   time.Duration(windowSec) * time.Second,
		MySQLDSN:     dsn,
		AmqpURL:      v.GetString("queue.amqp_url"),
		QueueEnabled: v.GetBool("queue.enabled"),
	}, nil
}
