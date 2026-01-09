package resilience

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// RetryConfig holds configuration for retry behavior
type RetryConfig struct {
	MaxAttempts   int
	InitialDelay  time.Duration
	MaxDelay      time.Duration
	BackoffFactor float64
	JitterFactor  float64
}

// DefaultRetryConfig returns sensible default retry configuration
func DefaultRetryConfig() RetryConfig {
	return RetryConfig{
		MaxAttempts:   3,
		InitialDelay:  time.Second,
		MaxDelay:      30 * time.Second,
		BackoffFactor: 2.0,
		JitterFactor:  0.1,
	}
}

// Retry executes an operation with exponential backoff
func Retry(ctx context.Context, config RetryConfig, operation func() error) error {
	var lastErr error

	for attempt := 1; attempt <= config.MaxAttempts; attempt++ {
		// Execute operation
		err := operation()
		if err == nil {
			return nil // Success
		}

		lastErr = err

		// Don't retry if this was the last attempt
		if attempt >= config.MaxAttempts {
			break
		}

		// Calculate delay with exponential backoff and jitter
		delay := calculateDelay(config, attempt)

		// Wait before retry, respecting context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("operation cancelled: %w", ctx.Err())
		case <-time.After(delay):
			continue
		}
	}

	return fmt.Errorf("operation failed after %d attempts: %w", config.MaxAttempts, lastErr)
}

// calculateDelay computes the delay for the next retry with exponential backoff and jitter
func calculateDelay(config RetryConfig, attempt int) time.Duration {
	// Calculate exponential backoff
	delay := float64(config.InitialDelay) * math.Pow(config.BackoffFactor, float64(attempt-1))

	// Add jitter to prevent thundering herd
	if config.JitterFactor > 0 {
		jitter := delay * config.JitterFactor * (rand.Float64()*2 - 1) // Random between -jitter and +jitter
		delay += jitter
	}

	// Cap at max delay
	if delay > float64(config.MaxDelay) {
		delay = float64(config.MaxDelay)
	}

	return time.Duration(delay)
}

// RetryableOperation wraps an operation with retry logic
type RetryableOperation struct {
	Name      string
	Operation func() error
	Config    RetryConfig
	OnRetry   func(attempt int, err error)
}

// Execute runs the operation with retries
func (r *RetryableOperation) Execute(ctx context.Context) error {
	var lastErr error

	for attempt := 1; attempt <= r.Config.MaxAttempts; attempt++ {
		err := r.Operation()
		if err == nil {
			return nil
		}

		lastErr = err

		// Call retry callback if provided
		if r.OnRetry != nil && attempt < r.Config.MaxAttempts {
			r.OnRetry(attempt, err)
		}

		// Don't wait after last attempt
		if attempt >= r.Config.MaxAttempts {
			break
		}

		delay := calculateDelay(r.Config, attempt)

		select {
		case <-ctx.Done():
			return fmt.Errorf("%s cancelled: %w", r.Name, ctx.Err())
		case <-time.After(delay):
			continue
		}
	}

	return fmt.Errorf("%s failed after %d attempts: %w", r.Name, r.Config.MaxAttempts, lastErr)
}

// =============================================================================
// Advanced Retry with cenkalti/backoff
// =============================================================================

// BackoffRetryOption configures backoff retry behavior
type BackoffRetryOption func(*backoffConfig)

type backoffConfig struct {
	maxElapsed   time.Duration
	maxRetries   uint64
	initialDelay time.Duration
	maxDelay     time.Duration
	multiplier   float64
	onRetry      func(err error, duration time.Duration)
	classifier   func(error) bool // returns true if error is retryable
}

// WithMaxElapsed sets the maximum total time for retries
func WithMaxElapsed(d time.Duration) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.maxElapsed = d
	}
}

// WithMaxRetries sets the maximum number of retry attempts
func WithMaxRetries(n uint64) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.maxRetries = n
	}
}

// WithInitialDelay sets the initial delay between retries
func WithInitialDelay(d time.Duration) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.initialDelay = d
	}
}

// WithMaxDelay sets the maximum delay between retries
func WithMaxDelay(d time.Duration) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.maxDelay = d
	}
}

// WithMultiplier sets the backoff multiplier
func WithMultiplier(m float64) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.multiplier = m
	}
}

// WithOnRetry sets a callback for each retry attempt
func WithOnRetry(fn func(err error, duration time.Duration)) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.onRetry = fn
	}
}

// WithRetryClassifier sets a function to determine if an error is retryable
func WithRetryClassifier(fn func(error) bool) BackoffRetryOption {
	return func(c *backoffConfig) {
		c.classifier = fn
	}
}

// RetryWithBackoff provides advanced retry with exponential backoff using cenkalti/backoff.
// This is more robust than the basic Retry function and supports:
// - Maximum elapsed time limit
// - Maximum retry count
// - Customizable backoff parameters
// - Retry classification (determine if error is retryable)
// - Context cancellation
func RetryWithBackoff(ctx context.Context, operation func() error, opts ...BackoffRetryOption) error {
	// Default configuration
	cfg := &backoffConfig{
		maxElapsed:   2 * time.Minute,
		maxRetries:   0, // unlimited by default
		initialDelay: time.Second,
		maxDelay:     30 * time.Second,
		multiplier:   2.0,
		classifier:   DefaultRetryClassifier,
	}

	// Apply options
	for _, opt := range opts {
		opt(cfg)
	}

	// Create exponential backoff
	b := backoff.NewExponentialBackOff()
	b.InitialInterval = cfg.initialDelay
	b.MaxInterval = cfg.maxDelay
	b.MaxElapsedTime = cfg.maxElapsed
	b.Multiplier = cfg.multiplier
	b.RandomizationFactor = 0.1 // 10% jitter

	// Wrap with max retries if specified
	var bo backoff.BackOff = b
	if cfg.maxRetries > 0 {
		bo = backoff.WithMaxRetries(b, cfg.maxRetries)
	}

	// Wrap with context
	bo = backoff.WithContext(bo, ctx)

	// Create operation wrapper for classification
	wrappedOp := func() error {
		err := operation()
		if err == nil {
			return nil
		}

		// Check if error is retryable
		if cfg.classifier != nil && !cfg.classifier(err) {
			// Permanent error - don't retry
			return backoff.Permanent(err)
		}

		return err
	}

	// Execute with optional notification
	if cfg.onRetry != nil {
		return backoff.RetryNotify(wrappedOp, bo, cfg.onRetry)
	}

	return backoff.Retry(wrappedOp, bo)
}

// DefaultRetryClassifier determines if an error is retryable.
// Network errors and timeouts are retryable; auth and validation errors are not.
func DefaultRetryClassifier(err error) bool {
	if err == nil {
		return false
	}

	// Network errors are retryable
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}

	// Connection refused, reset, etc. are retryable
	if errors.Is(err, net.ErrClosed) {
		return true
	}

	// Context cancelled/deadline exceeded are NOT retryable
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	// Default: retry unknown errors
	return true
}

// IsRetryable checks if an error should be retried
func IsRetryable(err error) bool {
	return DefaultRetryClassifier(err)
}

// PermanentError wraps an error to indicate it should not be retried
func PermanentError(err error) error {
	return backoff.Permanent(err)
}
