// Package telemetry provides OpenTelemetry tracing foundation for tako-cli.
// Tracing is disabled by default and can be enabled via environment variables.
package telemetry

import (
	"context"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var (
	tracer         trace.Tracer
	tracerProvider *sdktrace.TracerProvider
	initOnce       sync.Once
	enabled        bool
)

// Config holds telemetry configuration
type Config struct {
	// ServiceName is the name of the service (default: tako-cli)
	ServiceName string
	// ServiceVersion is the version of the service
	ServiceVersion string
	// Environment is the deployment environment (e.g., production, staging)
	Environment string
	// OTLPEndpoint is the OTLP collector endpoint (e.g., localhost:4317)
	OTLPEndpoint string
	// Debug enables stdout trace exporter for debugging
	Debug bool
}

// DefaultConfig returns the default telemetry configuration
func DefaultConfig() Config {
	return Config{
		ServiceName:    getEnvOrDefault("TAKO_SERVICE_NAME", "tako-cli"),
		ServiceVersion: getEnvOrDefault("TAKO_VERSION", "dev"),
		Environment:    getEnvOrDefault("TAKO_ENVIRONMENT", "development"),
		OTLPEndpoint:   os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Debug:          os.Getenv("TAKO_TRACE_DEBUG") == "1",
	}
}

// Init initializes the telemetry system.
// Call this early in main() if you want tracing enabled.
// If OTEL_EXPORTER_OTLP_ENDPOINT is not set, tracing is disabled (noop).
func Init(cfg Config) error {
	var err error
	initOnce.Do(func() {
		err = initTracer(cfg)
	})
	return err
}

// initTracer sets up the tracer provider
func initTracer(cfg Config) error {
	// Check if tracing should be enabled
	if cfg.OTLPEndpoint == "" && !cfg.Debug {
		// No endpoint configured, use noop tracer
		tracer = noop.NewTracerProvider().Tracer(cfg.ServiceName)
		enabled = false
		return nil
	}

	enabled = true

	// Create resource with service information
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
			attribute.String("environment", cfg.Environment),
		),
	)
	if err != nil {
		return err
	}

	// Create exporter based on configuration
	var exporter sdktrace.SpanExporter

	if cfg.Debug {
		// Use stdout exporter for debugging
		exporter, err = stdouttrace.New(
			stdouttrace.WithPrettyPrint(),
		)
		if err != nil {
			return err
		}
	} else if cfg.OTLPEndpoint != "" {
		// Use OTLP exporter
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		client := otlptracegrpc.NewClient(
			otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
			otlptracegrpc.WithInsecure(), // TODO: Add TLS config option
		)

		exporter, err = otlptrace.New(ctx, client)
		if err != nil {
			return err
		}
	}

	// Create tracer provider
	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // Sample everything for CLI tool
	)

	// Set global tracer provider
	otel.SetTracerProvider(tracerProvider)

	// Create tracer
	tracer = tracerProvider.Tracer(cfg.ServiceName)

	return nil
}

// Shutdown gracefully shuts down the tracer provider
func Shutdown(ctx context.Context) error {
	if tracerProvider != nil {
		return tracerProvider.Shutdown(ctx)
	}
	return nil
}

// IsEnabled returns true if tracing is enabled
func IsEnabled() bool {
	return enabled
}

// Tracer returns the global tracer instance
func Tracer() trace.Tracer {
	if tracer == nil {
		// Return noop tracer if not initialized
		return noop.NewTracerProvider().Tracer("tako-cli")
	}
	return tracer
}

// StartSpan starts a new span with the given name
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, opts...)
}

// SpanFromContext returns the current span from context
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}

// =============================================================================
// Convenience functions for common operations
// =============================================================================

// TraceSSH starts a span for SSH operations
func TraceSSH(ctx context.Context, host, command string) (context.Context, trace.Span) {
	return StartSpan(ctx, "ssh.execute",
		trace.WithAttributes(
			attribute.String("ssh.host", host),
			attribute.String("ssh.command", truncate(command, 100)),
		),
	)
}

// TraceDeploy starts a span for deployment operations
func TraceDeploy(ctx context.Context, project, service, environment string) (context.Context, trace.Span) {
	return StartSpan(ctx, "deploy.service",
		trace.WithAttributes(
			attribute.String("deploy.project", project),
			attribute.String("deploy.service", service),
			attribute.String("deploy.environment", environment),
		),
	)
}

// TraceHealthCheck starts a span for health check operations
func TraceHealthCheck(ctx context.Context, service, domain string) (context.Context, trace.Span) {
	return StartSpan(ctx, "health.check",
		trace.WithAttributes(
			attribute.String("health.service", service),
			attribute.String("health.domain", domain),
		),
	)
}

// TraceHTTP starts a span for HTTP operations
func TraceHTTP(ctx context.Context, method, url string) (context.Context, trace.Span) {
	return StartSpan(ctx, "http.request",
		trace.WithAttributes(
			attribute.String("http.method", method),
			attribute.String("http.url", url),
		),
	)
}

// TraceState starts a span for state operations
func TraceState(ctx context.Context, operation, project string) (context.Context, trace.Span) {
	return StartSpan(ctx, "state."+operation,
		trace.WithAttributes(
			attribute.String("state.operation", operation),
			attribute.String("state.project", project),
		),
	)
}

// RecordError records an error on the current span
func RecordError(ctx context.Context, err error) {
	span := SpanFromContext(ctx)
	if span != nil {
		span.RecordError(err)
	}
}

// SetAttribute sets an attribute on the current span
func SetAttribute(ctx context.Context, key string, value interface{}) {
	span := SpanFromContext(ctx)
	if span == nil {
		return
	}

	switch v := value.(type) {
	case string:
		span.SetAttributes(attribute.String(key, v))
	case int:
		span.SetAttributes(attribute.Int(key, v))
	case int64:
		span.SetAttributes(attribute.Int64(key, v))
	case float64:
		span.SetAttributes(attribute.Float64(key, v))
	case bool:
		span.SetAttributes(attribute.Bool(key, v))
	}
}

// =============================================================================
// Helper functions
// =============================================================================

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

func getEnvOrDefault(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}
