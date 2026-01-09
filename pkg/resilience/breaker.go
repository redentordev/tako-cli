// Package resilience provides reliability patterns for tako-cli operations.
// This file provides a circuit breaker wrapper using sony/gobreaker.
package resilience

import (
	"fmt"
	"time"

	"github.com/sony/gobreaker/v2"
)

// ServiceBreaker wraps gobreaker with tako-cli defaults and provides
// a simpler API for common use cases.
type ServiceBreaker struct {
	cb   *gobreaker.CircuitBreaker[any]
	name string
}

// BreakerOption configures a ServiceBreaker
type BreakerOption func(*gobreaker.Settings)

// WithMaxRequests sets the maximum number of requests allowed in half-open state
func WithMaxRequests(n uint32) BreakerOption {
	return func(s *gobreaker.Settings) {
		s.MaxRequests = n
	}
}

// WithInterval sets the cyclic period of the closed state for clearing counts
func WithInterval(d time.Duration) BreakerOption {
	return func(s *gobreaker.Settings) {
		s.Interval = d
	}
}

// WithTimeout sets the period of the open state before becoming half-open
func WithTimeout(d time.Duration) BreakerOption {
	return func(s *gobreaker.Settings) {
		s.Timeout = d
	}
}

// WithFailureThreshold sets the number of consecutive failures before opening
func WithFailureThreshold(n uint32) BreakerOption {
	return func(s *gobreaker.Settings) {
		s.ReadyToTrip = func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= n
		}
	}
}

// WithOnStateChange sets a callback for state changes
func WithOnStateChange(fn func(name string, from, to string)) BreakerOption {
	return func(s *gobreaker.Settings) {
		s.OnStateChange = func(name string, from, to gobreaker.State) {
			fn(name, from.String(), to.String())
		}
	}
}

// NewServiceBreaker creates a new circuit breaker with sensible defaults for tako-cli.
// Default settings:
// - MaxRequests: 3 (requests allowed in half-open state)
// - Interval: 60s (stat collection window in closed state)
// - Timeout: 30s (how long to stay open before trying half-open)
// - FailureThreshold: 5 (consecutive failures to trip)
func NewServiceBreaker(name string, opts ...BreakerOption) *ServiceBreaker {
	settings := gobreaker.Settings{
		Name:        name,
		MaxRequests: 3,
		Interval:    60 * time.Second,
		Timeout:     30 * time.Second,
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			return counts.ConsecutiveFailures >= 5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			// Default: silent state changes
		},
	}

	// Apply options
	for _, opt := range opts {
		opt(&settings)
	}

	return &ServiceBreaker{
		cb:   gobreaker.NewCircuitBreaker[any](settings),
		name: name,
	}
}

// Execute runs an operation through the circuit breaker.
// Returns an error if the circuit is open or if the operation fails.
func (b *ServiceBreaker) Execute(fn func() error) error {
	_, err := b.cb.Execute(func() (any, error) {
		return nil, fn()
	})
	return err
}

// ExecuteWithResult runs an operation that returns a result through the circuit breaker.
func ExecuteWithResult[T any](b *ServiceBreaker, fn func() (T, error)) (T, error) {
	// Create a new typed circuit breaker for the operation
	result, err := b.cb.Execute(func() (any, error) {
		return fn()
	})
	if err != nil {
		var zero T
		return zero, err
	}
	return result.(T), nil
}

// State returns the current state of the circuit breaker
func (b *ServiceBreaker) State() string {
	return b.cb.State().String()
}

// Name returns the name of the circuit breaker
func (b *ServiceBreaker) Name() string {
	return b.name
}

// Counts returns the current counts (requests, successes, failures, etc.)
func (b *ServiceBreaker) Counts() gobreaker.Counts {
	return b.cb.Counts()
}

// IsOpen returns true if the circuit is open (blocking requests)
func (b *ServiceBreaker) IsOpen() bool {
	return b.cb.State() == gobreaker.StateOpen
}

// IsClosed returns true if the circuit is closed (allowing requests)
func (b *ServiceBreaker) IsClosed() bool {
	return b.cb.State() == gobreaker.StateClosed
}

// IsHalfOpen returns true if the circuit is half-open (testing recovery)
func (b *ServiceBreaker) IsHalfOpen() bool {
	return b.cb.State() == gobreaker.StateHalfOpen
}

// ErrCircuitOpen is returned when the circuit breaker is open
var ErrCircuitOpen = fmt.Errorf("circuit breaker is open")

// Common breakers for different operation types
var (
	// SSHBreaker protects SSH operations
	sshBreaker *ServiceBreaker

	// HTTPBreaker protects HTTP operations
	httpBreaker *ServiceBreaker

	// DeployBreaker protects deployment operations
	deployBreaker *ServiceBreaker
)

// GetSSHBreaker returns the shared SSH circuit breaker
func GetSSHBreaker() *ServiceBreaker {
	if sshBreaker == nil {
		sshBreaker = NewServiceBreaker("ssh",
			WithFailureThreshold(3),
			WithTimeout(60*time.Second),
		)
	}
	return sshBreaker
}

// GetHTTPBreaker returns the shared HTTP circuit breaker
func GetHTTPBreaker() *ServiceBreaker {
	if httpBreaker == nil {
		httpBreaker = NewServiceBreaker("http",
			WithFailureThreshold(5),
			WithTimeout(30*time.Second),
		)
	}
	return httpBreaker
}

// GetDeployBreaker returns the shared deployment circuit breaker
func GetDeployBreaker() *ServiceBreaker {
	if deployBreaker == nil {
		deployBreaker = NewServiceBreaker("deploy",
			WithFailureThreshold(2),
			WithTimeout(120*time.Second),
		)
	}
	return deployBreaker
}
