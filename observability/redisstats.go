package observability

import (
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"
)

// RedisStatsOptions configures Prometheus Redis pool stats registration.
type RedisStatsOptions struct {
	Registerer prometheus.Registerer
}

var redisStatsRegistrations sync.Map

// RegisterRedisPoolStats registers Prometheus collectors for Redis connection pool stats.
func RegisterRedisPoolStats(serviceName string, statFunc func() *goredis.PoolStats, opts RedisStatsOptions) error {
	if statFunc == nil {
		return nil
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.DefaultRegisterer
	}

	key := fmt.Sprintf("%T:%p:%s", opts.Registerer, opts.Registerer, metricPrefix(serviceName))
	onceValue, _ := redisStatsRegistrations.LoadOrStore(key, &sync.Once{})
	once, _ := onceValue.(*sync.Once)

	var registerErr error
	once.Do(func() {
		prefix := metricPrefix(serviceName)

		registerErr = registerRedisGauge(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_total_connections", prefix), "Number of total Redis connections in the pool.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.TotalConns)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisGauge(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_idle_connections", prefix), "Number of idle Redis connections in the pool.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.IdleConns)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisGauge(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_pending_requests", prefix), "Number of Redis requests currently waiting for a pooled connection.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.PendingRequests)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_hits_total", prefix), "Count of Redis requests that reused an idle pooled connection.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.Hits)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_misses_total", prefix), "Count of Redis requests that could not reuse an idle pooled connection.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.Misses)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_wait_count_total", prefix), "Count of Redis requests that had to wait for a pooled connection.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.WaitCount)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_wait_duration_seconds_total", prefix), "Total time spent waiting for a Redis pooled connection.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.WaitDurationNs) / float64(time.Second)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_timeouts_total", prefix), "Count of Redis connection pool wait timeouts.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.Timeouts)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_unusable_total", prefix), "Count of Redis pooled connections found to be unusable.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.Unusable)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerRedisCounter(opts.Registerer, statFunc, fmt.Sprintf("%s_redis_pool_stale_connections_total", prefix), "Count of stale Redis pooled connections removed.", func(stats *goredis.PoolStats) float64 {
			return float64(stats.StaleConns)
		})
	})

	return registerErr
}

func registerRedisGauge(registerer prometheus.Registerer, statFunc func() *goredis.PoolStats, name, help string, valueFunc func(*goredis.PoolStats) float64) error {
	collector := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: name,
			Help: help,
		},
		func() float64 {
			return redisStatValue(statFunc, valueFunc)
		},
	)
	if err := registerer.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return nil
		}
		return err
	}
	return nil
}

func registerRedisCounter(registerer prometheus.Registerer, statFunc func() *goredis.PoolStats, name, help string, valueFunc func(*goredis.PoolStats) float64) error {
	collector := prometheus.NewCounterFunc(
		prometheus.CounterOpts{
			Name: name,
			Help: help,
		},
		func() float64 {
			return redisStatValue(statFunc, valueFunc)
		},
	)
	if err := registerer.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return nil
		}
		return err
	}
	return nil
}

func redisStatValue(statFunc func() *goredis.PoolStats, valueFunc func(*goredis.PoolStats) float64) float64 {
	if statFunc == nil || valueFunc == nil {
		return 0
	}
	stats := statFunc()
	if stats == nil {
		return 0
	}
	return valueFunc(stats)
}
