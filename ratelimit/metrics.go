package ratelimit

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
)

type metrics struct {
	hitsTotal *prometheus.CounterVec
}

func newMetrics(cfg Config) *metrics {
	if strings.TrimSpace(cfg.ServiceName) == "" {
		return nil
	}

	registerer := cfg.Registerer
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}

	hitsTotal, err := registerCounterVec(
		registerer,
		prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: fmt.Sprintf("%s_rate_limit_hits_total", metricPrefix(cfg.ServiceName)),
				Help: fmt.Sprintf("Count of rate limiting decisions handled by %s.", cfg.ServiceName),
			},
			[]string{"route", "action"},
		),
	)
	if err != nil {
		return nil
	}

	return &metrics{hitsTotal: hitsTotal}
}

func (m *metrics) record(route, action string) {
	if m == nil || m.hitsTotal == nil {
		return
	}
	if strings.TrimSpace(route) == "" {
		route = "unknown"
	}
	if strings.TrimSpace(action) == "" {
		action = "unknown"
	}
	m.hitsTotal.WithLabelValues(route, action).Inc()
}

func metricPrefix(serviceName string) string {
	serviceName = strings.TrimSpace(serviceName)
	if serviceName == "" {
		return "service"
	}

	var builder strings.Builder
	for index, runeValue := range serviceName {
		switch {
		case unicode.IsLetter(runeValue), unicode.IsDigit(runeValue):
			builder.WriteRune(unicode.ToLower(runeValue))
		default:
			builder.WriteByte('_')
		}
		if index == 0 && unicode.IsDigit(runeValue) {
			builder.WriteByte('_')
		}
	}

	prefix := strings.Trim(builder.String(), "_")
	if prefix == "" {
		return "service"
	}
	if prefix[0] >= '0' && prefix[0] <= '9' {
		return "service_" + prefix
	}
	return prefix
}

func registerCounterVec(registerer prometheus.Registerer, collector *prometheus.CounterVec) (*prometheus.CounterVec, error) {
	if err := registerer.Register(collector); err != nil {
		alreadyRegistered, ok := err.(prometheus.AlreadyRegisteredError)
		if !ok {
			return nil, err
		}
		existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec)
		if !ok {
			return nil, err
		}
		return existing, nil
	}
	return collector, nil
}
