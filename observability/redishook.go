package observability

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
)

// RedisCommandMetricsOptions configures Prometheus Redis command metric registration.
type RedisCommandMetricsOptions struct {
	Registerer      prometheus.Registerer
	DurationBuckets []float64
}

// NewRedisCommandHook registers Redis command metrics and returns a hook that records them.
func NewRedisCommandHook(serviceName string, opts RedisCommandMetricsOptions) (goredis.Hook, error) {
	opts = opts.withDefaults()
	prefix := metricPrefix(serviceName)

	commandsTotal, err := registerCounterVec(
		opts.Registerer,
		prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: prefix + "_redis_commands_total",
				Help: "Count of Redis commands executed by the service.",
			},
			[]string{"command", "status"},
		),
	)
	if err != nil {
		return nil, err
	}

	commandDuration, err := registerHistogramVec(
		opts.Registerer,
		prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    prefix + "_redis_command_duration_seconds",
				Help:    "Duration of Redis commands executed by the service.",
				Buckets: opts.DurationBuckets,
			},
			[]string{"command", "status"},
		),
	)
	if err != nil {
		return nil, err
	}

	return &redisCommandHook{
		commandsTotal:   commandsTotal,
		commandDuration: commandDuration,
	}, nil
}

func (opts RedisCommandMetricsOptions) withDefaults() RedisCommandMetricsOptions {
	if opts.Registerer == nil {
		opts.Registerer = prometheus.DefaultRegisterer
	}
	if len(opts.DurationBuckets) == 0 {
		opts.DurationBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1}
	}
	return opts
}

type redisCommandHook struct {
	commandsTotal   *prometheus.CounterVec
	commandDuration *prometheus.HistogramVec
}

var _ goredis.Hook = (*redisCommandHook)(nil)

func (hook *redisCommandHook) DialHook(next goredis.DialHook) goredis.DialHook {
	return next
}

func (hook *redisCommandHook) ProcessHook(next goredis.ProcessHook) goredis.ProcessHook {
	return func(ctx context.Context, cmd goredis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		hook.record(redisCommandName(cmd), redisCommandStatus(err), time.Since(start))
		return err
	}
}

func (hook *redisCommandHook) ProcessPipelineHook(next goredis.ProcessPipelineHook) goredis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []goredis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		hook.record(redisPipelineName(cmds), redisCommandStatus(err), time.Since(start))
		return err
	}
}

func (hook *redisCommandHook) record(command, status string, duration time.Duration) {
	if hook == nil {
		return
	}
	hook.commandsTotal.WithLabelValues(command, status).Inc()
	hook.commandDuration.WithLabelValues(command, status).Observe(duration.Seconds())
}

func redisCommandName(cmd goredis.Cmder) string {
	if cmd == nil {
		return "unknown"
	}
	name := strings.TrimSpace(cmd.Name())
	if name == "" {
		return "unknown"
	}
	return strings.ToLower(name)
}

func redisPipelineName(_ []goredis.Cmder) string {
	return "pipeline"
}

func redisCommandStatus(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, goredis.Nil):
		return "nil"
	default:
		return "error"
	}
}
