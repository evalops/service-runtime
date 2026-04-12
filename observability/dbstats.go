// Package observability provides HTTP request logging, Prometheus metrics, and wide event tracking.
package observability

import (
	"database/sql"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

// DBStatsOptions configures database stats registration with a custom Prometheus registerer.
type DBStatsOptions struct {
	Registerer prometheus.Registerer
}

var dbStatsRegistrations sync.Map

// RegisterDBStats registers Prometheus gauges that expose sql.DBStats for the given service.
func RegisterDBStats(serviceName string, statFunc func() sql.DBStats, opts DBStatsOptions) error {
	if statFunc == nil {
		return nil
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.DefaultRegisterer
	}

	key := fmt.Sprintf("%T:%p:%s", opts.Registerer, opts.Registerer, metricPrefix(serviceName))
	onceValue, _ := dbStatsRegistrations.LoadOrStore(key, &sync.Once{})
	once, ok := onceValue.(*sync.Once)
	if !ok {
		return fmt.Errorf("db_stats_registration_invalid: %T", onceValue)
	}

	var registerErr error
	once.Do(func() {
		registerErr = registerDBGauge(opts.Registerer, fmt.Sprintf("%s_db_open_connections", metricPrefix(serviceName)), "Number of open database connections.", func() float64 {
			return float64(statFunc().OpenConnections)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerDBGauge(opts.Registerer, fmt.Sprintf("%s_db_in_use_connections", metricPrefix(serviceName)), "Number of database connections currently in use.", func() float64 {
			return float64(statFunc().InUse)
		})
		if registerErr != nil {
			return
		}
		registerErr = registerDBGauge(opts.Registerer, fmt.Sprintf("%s_db_idle_connections", metricPrefix(serviceName)), "Number of idle database connections.", func() float64 {
			return float64(statFunc().Idle)
		})
	})

	return registerErr
}

func registerDBGauge(registerer prometheus.Registerer, name, help string, valueFunc func() float64) error {
	collector := prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: name,
			Help: help,
		},
		valueFunc,
	)
	if err := registerer.Register(collector); err != nil {
		if _, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return nil
		}
		return err
	}
	return nil
}
