package runtime

import (
	"fmt"
	"log/slog"

	"github.com/caarlos0/env/v11"
)

// The contract's ports. They are the same for every service, so the chart,
// the network policy and the scrape config never ask a service for them.
const (
	// HTTPPort serves the service's traffic.
	HTTPPort = 8080
	// ProbePort serves /healthz and /readyz.
	ProbePort = 8081
	// MetricsPort serves /metrics.
	MetricsPort = 9464
)

// Config is the contract's configuration, read from the names the
// metsa-service chart injects. The connection settings are empty when the
// service's definition does not enable them; a service checks the ones it
// needs. The remaining OTEL_* variables are read by the OTel SDK itself.
type Config struct {
	Environment string     `env:"ENVIRONMENT" envDefault:"local"`
	LogLevel    slog.Level `env:"LOG_LEVEL"   envDefault:"info"`
	// ServiceName names the service in traces, metrics and logs. It is never
	// set in code, so telemetry matches the deployment.
	ServiceName string `env:"OTEL_SERVICE_NAME,required,notEmpty"`
	// ServiceIdentity is <namespace>/<serviceaccount>, the NATS user.
	ServiceIdentity string `env:"SERVICE_IDENTITY"`
	DatabaseURL     string `env:"DATABASE_URL"`
	NATSURL         string `env:"NATS_URL"`
	// NATSTokenFile holds a projected token that rotates hourly: read it on
	// every connect, never once at start.
	NATSTokenFile string `env:"NATS_TOKEN_FILE"`
	RedisURL      string `env:"REDIS_URL"`
}

func loadConfig() (Config, error) {
	cfg, err := env.ParseAs[Config]()
	if err != nil {
		return Config{}, fmt.Errorf("load runtime config: %w", err)
	}
	return cfg, nil
}
