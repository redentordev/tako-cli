package resilience

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"
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
