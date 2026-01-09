// Package httputil provides shared HTTP client utilities with optimized connection pooling.
// This prevents TIME_WAIT socket accumulation and improves performance.
package httputil

import (
	"crypto/tls"
	"net/http"
	"sync"
	"time"
)

var (
	defaultClient     *http.Client
	defaultClientOnce sync.Once

	// insecureClient skips TLS verification (use with caution)
	insecureClient     *http.Client
	insecureClientOnce sync.Once
)

// DefaultClient returns a shared HTTP client with optimized connection pooling.
// The client is safe for concurrent use and reuses connections efficiently.
func DefaultClient() *http.Client {
	defaultClientOnce.Do(func() {
		defaultClient = newOptimizedClient(30*time.Second, false)
	})
	return defaultClient
}

// InsecureClient returns a shared HTTP client that skips TLS verification.
// Use only when connecting to servers with self-signed certificates.
func InsecureClient() *http.Client {
	insecureClientOnce.Do(func() {
		insecureClient = newOptimizedClient(30*time.Second, true)
	})
	return insecureClient
}

// NewClientWithTimeout creates a new HTTP client with the specified timeout.
// The client shares the optimized transport for connection reuse.
func NewClientWithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: DefaultClient().Transport,
	}
}

// NewInsecureClientWithTimeout creates a new HTTP client that skips TLS verification.
// Use only when connecting to servers with self-signed certificates.
func NewInsecureClientWithTimeout(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: InsecureClient().Transport,
	}
}

// newOptimizedClient creates an HTTP client with optimized transport settings.
func newOptimizedClient(timeout time.Duration, insecure bool) *http.Client {
	// Clone the default transport to get sensible defaults
	transport := http.DefaultTransport.(*http.Transport).Clone()

	// Optimize connection pooling to prevent TIME_WAIT accumulation
	transport.MaxIdleConns = 100
	transport.MaxConnsPerHost = 100
	transport.MaxIdleConnsPerHost = 100
	transport.IdleConnTimeout = 90 * time.Second

	// Enable HTTP/2 by default for better performance
	transport.ForceAttemptHTTP2 = true

	// Set reasonable timeouts
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.ExpectContinueTimeout = 1 * time.Second

	if insecure {
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: true,
		}
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

// Get performs a GET request using the default client
func Get(url string) (*http.Response, error) {
	return DefaultClient().Get(url)
}

// Head performs a HEAD request using the default client
func Head(url string) (*http.Response, error) {
	return DefaultClient().Head(url)
}
