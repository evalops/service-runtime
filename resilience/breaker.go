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

// Circuit breaker states.
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
	FailureThreshold int                        // consecutive failures before opening (default: 5)
	ResetTimeout     time.Duration              // time in open state before half-open probe (default: 30s)
	HalfOpenMax      int                        // max concurrent probes in half-open (default: 1)
	OnStateChange    func(from, to BreakerState) // optional callback on state transitions
	Clock            func() time.Time            // optional clock for testing (default: time.Now)
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

	timeNow       func() time.Time
	onStateChange func(from, to BreakerState)
}

// NewBreaker creates a circuit breaker with the given configuration.
func NewBreaker(cfg BreakerConfig) *Breaker {
	cfg = cfg.withDefaults()
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Breaker{
		cfg:           cfg,
		state:         StateClosed,
		timeNow:       clock,
		onStateChange: cfg.OnStateChange,
	}
}

// State returns the current state of the circuit breaker. If the breaker is
// open and the reset timeout has elapsed, this reports StateHalfOpen.
func (b *Breaker) State() BreakerState {
	b.mu.Lock()
	state, from, to, changed := b.stateLocked()
	cb := b.onStateChange
	b.mu.Unlock()

	if changed && cb != nil {
		cb(from, to)
	}
	return state
}

// stateLocked returns the current state, promoting Open → HalfOpen when the
// reset timeout has elapsed. Caller must hold b.mu.
func (b *Breaker) stateLocked() (state, from, to BreakerState, changed bool) {
	if b.state == StateOpen && b.timeNow().Sub(b.openedAt) >= b.cfg.ResetTimeout {
		from = StateOpen
		to = StateHalfOpen
		changed = true
		b.state = StateHalfOpen
		b.halfOpenCount = 0
	}
	return b.state, from, to, changed
}

// Reset forces the breaker back to Closed, clearing all counters.
func (b *Breaker) Reset() {
	b.mu.Lock()
	prev := b.state
	b.state = StateClosed
	b.consecutiveFail = 0
	b.halfOpenCount = 0
	cb := b.onStateChange
	b.mu.Unlock()

	if cb != nil && prev != StateClosed {
		cb(prev, StateClosed)
	}
}

// Do executes fn through the circuit breaker. If the circuit is open, it
// returns ErrCircuitOpen without calling fn. Failures and successes drive state
// transitions.
func (b *Breaker) Do(ctx context.Context, fn func(context.Context) error) error {
	wasProbe, err := b.before()
	if err != nil {
		return err
	}
	err = fn(ctx)
	b.after(wasProbe, err)
	return err
}

// before checks whether the call is permitted and returns ErrCircuitOpen if not.
// It returns wasProbe=true when the call is a half-open probe.
func (b *Breaker) before() (wasProbe bool, err error) {
	b.mu.Lock()
	state, from, to, changed := b.stateLocked()
	cb := b.onStateChange
	switch state {
	case StateClosed:
		b.mu.Unlock()
		if changed && cb != nil {
			cb(from, to)
		}
		return false, nil
	case StateOpen:
		b.mu.Unlock()
		if changed && cb != nil {
			cb(from, to)
		}
		return false, ErrCircuitOpen
	case StateHalfOpen:
		if b.halfOpenCount >= b.cfg.HalfOpenMax {
			b.mu.Unlock()
			if changed && cb != nil {
				cb(from, to)
			}
			return false, ErrCircuitOpen
		}
		b.halfOpenCount++
		b.mu.Unlock()
		if changed && cb != nil {
			cb(from, to)
		}
		return true, nil
	default:
		b.mu.Unlock()
		if changed && cb != nil {
			cb(from, to)
		}
		return false, ErrCircuitOpen
	}
}

// after records the outcome of a call and transitions state accordingly.
// If wasProbe is true and the state has already moved out of HalfOpen (e.g.
// because a concurrent probe resolved first), the outcome is discarded to
// avoid corrupting the new state.
func (b *Breaker) after(wasProbe bool, err error) {
	b.mu.Lock()

	// Another probe already transitioned the breaker out of HalfOpen.
	// Discard this outcome so it doesn't pollute the new state.
	if wasProbe && b.state != StateHalfOpen {
		b.mu.Unlock()
		return
	}

	var from, to BreakerState
	var changed bool
	if err == nil {
		from, to, changed = b.onSuccess()
	} else {
		from, to, changed = b.onFailure()
	}
	cb := b.onStateChange
	b.mu.Unlock()

	if changed && cb != nil {
		cb(from, to)
	}
}

// onSuccess records a successful outcome and returns any state transition.
// Caller must hold b.mu.
func (b *Breaker) onSuccess() (from, to BreakerState, changed bool) {
	prev := b.state
	switch b.state {
	case StateClosed:
		b.consecutiveFail = 0
	case StateHalfOpen:
		b.state = StateClosed
		b.consecutiveFail = 0
		b.halfOpenCount = 0
	}
	return prev, b.state, prev != b.state
}

// onFailure records a failed outcome and returns any state transition.
// Caller must hold b.mu.
func (b *Breaker) onFailure() (from, to BreakerState, changed bool) {
	prev := b.state
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
	return prev, b.state, prev != b.state
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
