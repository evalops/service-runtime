// Package resilience provides production-grade resilience primitives: a circuit
// breaker and exponential-backoff retry with jitter.
package resilience

import (
	"context"
	"errors"
	"sync"
	"time"
)

// BreakerState represents the current state of a circuit breaker.
type BreakerState int

const (
	StateClosed   BreakerState = iota // normal operation
	StateOpen                         // all calls rejected
	StateHalfOpen                     // limited probes allowed
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// ErrCircuitOpen is returned when a call is attempted on an open circuit.
var ErrCircuitOpen = errors.New("circuit_open")

// BreakerConfig tunes circuit breaker behaviour.
type BreakerConfig struct {
	FailureThreshold int           // consecutive failures before opening (default: 5)
	ResetTimeout     time.Duration // time in open state before half-open probe (default: 30s)
	HalfOpenMax      int           // max concurrent probes in half-open (default: 1)
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold < 1 {
		c.FailureThreshold = 5
	}
	if c.ResetTimeout <= 0 {
		c.ResetTimeout = 30 * time.Second
	}
	if c.HalfOpenMax < 1 {
		c.HalfOpenMax = 1
	}
	return c
}

// Breaker implements the circuit breaker pattern with three states: Closed,
// Open, and HalfOpen.
type Breaker struct {
	cfg BreakerConfig

	mu              sync.Mutex
	state           BreakerState
	consecutiveFail int
	openedAt        time.Time
	halfOpenCount   int

	// timeNow is injectable for testing.
	timeNow func() time.Time
}

// NewBreaker creates a circuit breaker with the given configuration.
func NewBreaker(cfg BreakerConfig) *Breaker {
	cfg = cfg.withDefaults()
	return &Breaker{
		cfg:     cfg,
		state:   StateClosed,
		timeNow: time.Now,
	}
}

// State returns the current state of the circuit breaker. If the breaker is
// open and the reset timeout has elapsed, this reports StateHalfOpen.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stateLocked()
}

// stateLocked returns the current state, promoting Open → HalfOpen when the
// reset timeout has elapsed. Caller must hold b.mu.
func (b *Breaker) stateLocked() BreakerState {
	if b.state == StateOpen && b.timeNow().Sub(b.openedAt) >= b.cfg.ResetTimeout {
		b.state = StateHalfOpen
		b.halfOpenCount = 0
	}
	return b.state
}

// Reset forces the breaker back to Closed, clearing all counters.
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = StateClosed
	b.consecutiveFail = 0
	b.halfOpenCount = 0
}

// Do executes fn through the circuit breaker. If the circuit is open, it
// returns ErrCircuitOpen without calling fn. Failures and successes drive state
// transitions.
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	if err := b.before(); err != nil {
		return err
	}
	err := fn(ctx)
	b.after(err)
	return err
}

// before checks whether the call is permitted and returns ErrCircuitOpen if not.
func (b *Breaker) before() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.stateLocked() {
	case StateClosed:
		return nil
	case StateOpen:
		return ErrCircuitOpen
	case StateHalfOpen:
		if b.halfOpenCount >= b.cfg.HalfOpenMax {
			return ErrCircuitOpen
		}
		b.halfOpenCount++
		return nil
	default:
		return ErrCircuitOpen
	}
}

// after records the outcome of a call and transitions state accordingly.
func (b *Breaker) after(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err == nil {
		b.onSuccess()
		return
	}
	b.onFailure()
}

func (b *Breaker) onSuccess() {
	switch b.state {
	case StateClosed:
		b.consecutiveFail = 0
	case StateHalfOpen:
		b.state = StateClosed
		b.consecutiveFail = 0
		b.halfOpenCount = 0
	}
}

func (b *Breaker) onFailure() {
	switch b.state {
	case StateClosed:
		b.consecutiveFail++
		if b.consecutiveFail >= b.cfg.FailureThreshold {
			b.state = StateOpen
			b.openedAt = b.timeNow()
		}
	case StateHalfOpen:
		b.state = StateOpen
		b.openedAt = b.timeNow()
		b.halfOpenCount = 0
	}
}

// DoValue executes a value-returning function through a Breaker. This is a
// standalone generic function because Go does not support generic methods.
func DoValue[T any](ctx context.Context, b *Breaker, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	var result T
	err := b.Do(ctx, func(ctx context.Context) error {
		v, e := fn(ctx)
		if e != nil {
			return e
		}
		result = v
		return nil
	})
	if err != nil {
		return zero, err
	}
	return result, nil
}
