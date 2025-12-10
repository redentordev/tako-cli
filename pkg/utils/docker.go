package utils

import (
	"fmt"
	"strings"
)

// DockerLabelBuilder helps build Docker labels in a type-safe way
type DockerLabelBuilder struct {
	labels []string
}

// NewDockerLabelBuilder creates a new label builder
func NewDockerLabelBuilder() *DockerLabelBuilder {
	return &DockerLabelBuilder{
		labels: make([]string, 0),
	}
}

// Add adds a label with key and value
func (b *DockerLabelBuilder) Add(key, value string) *DockerLabelBuilder {
	// Escape double quotes in value
	escapedValue := strings.ReplaceAll(value, "\"", "\\\"")
	b.labels = append(b.labels, fmt.Sprintf("--label %s=\"%s\"", key, escapedValue))
	return b
}

// AddWithQuotes adds a label with the value wrapped in the specified quote type
// quoteType can be "single" or "double"
func (b *DockerLabelBuilder) AddWithQuotes(key, value, quoteType string) *DockerLabelBuilder {
	var label string
	if quoteType == "single" {
		label = fmt.Sprintf("--label '%s=%s'", key, value)
	} else {
		label = fmt.Sprintf("--label \"%s=%s\"", key, value)
	}
	b.labels = append(b.labels, label)
	return b
}

// AddRaw adds a raw label string (use with caution)
func (b *DockerLabelBuilder) AddRaw(label string) *DockerLabelBuilder {
	b.labels = append(b.labels, label)
	return b
}

// AddIf conditionally adds a label if condition is true
func (b *DockerLabelBuilder) AddIf(condition bool, key, value string) *DockerLabelBuilder {
	if condition {
		return b.Add(key, value)
	}
	return b
}

// Build returns the labels as a space-separated string
func (b *DockerLabelBuilder) Build() string {
	return strings.Join(b.labels, " ")
}

// BuildSlice returns the labels as a slice
func (b *DockerLabelBuilder) BuildSlice() []string {
	return b.labels
}

// Count returns the number of labels
func (b *DockerLabelBuilder) Count() int {
	return len(b.labels)
}

// TraefikLabelBuilder specifically for Traefik labels
type TraefikLabelBuilder struct {
	*DockerLabelBuilder
	routerName  string
	serviceName string
}

// NewTraefikLabelBuilder creates a builder for Traefik labels
func NewTraefikLabelBuilder(routerName, serviceName string) *TraefikLabelBuilder {
	return &TraefikLabelBuilder{
		DockerLabelBuilder: NewDockerLabelBuilder(),
		routerName:         routerName,
		serviceName:        serviceName,
	}
}

// Enable enables Traefik for this container
func (t *TraefikLabelBuilder) Enable() *TraefikLabelBuilder {
	t.Add("traefik.enable", "true")
	return t
}

// HostRule adds a Host() rule for the router
func (t *TraefikLabelBuilder) HostRule(domain string) *TraefikLabelBuilder {
	rule := fmt.Sprintf("Host(\"%s\")", domain)
	key := fmt.Sprintf("traefik.http.routers.%s.rule", t.routerName)
	t.AddWithQuotes(key, rule, "single")
	return t
}

// Entrypoints sets the entrypoints for the router
func (t *TraefikLabelBuilder) Entrypoints(entrypoints ...string) *TraefikLabelBuilder {
	key := fmt.Sprintf("traefik.http.routers.%s.entrypoints", t.routerName)
	t.Add(key, strings.Join(entrypoints, ","))
	return t
}

// TLS enables TLS for the router
func (t *TraefikLabelBuilder) TLS(certResolver string) *TraefikLabelBuilder {
	t.Add(fmt.Sprintf("traefik.http.routers.%s.tls", t.routerName), "true")
	if certResolver != "" {
		t.Add(fmt.Sprintf("traefik.http.routers.%s.tls.certresolver", t.routerName), certResolver)
	}
	return t
}

// Port sets the service port
func (t *TraefikLabelBuilder) Port(port int) *TraefikLabelBuilder {
	key := fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", t.serviceName)
	t.Add(key, fmt.Sprintf("%d", port))
	return t
}

// HealthCheck adds health check configuration
func (t *TraefikLabelBuilder) HealthCheck(path string, interval string) *TraefikLabelBuilder {
	if path != "" {
		t.Add(fmt.Sprintf("traefik.http.services.%s.loadbalancer.healthcheck.path", t.serviceName), path)
	}
	if interval != "" {
		t.Add(fmt.Sprintf("traefik.http.services.%s.loadbalancer.healthcheck.interval", t.serviceName), interval)
	}
	return t
}

// Priority sets the router priority
func (t *TraefikLabelBuilder) Priority(priority int) *TraefikLabelBuilder {
	key := fmt.Sprintf("traefik.http.routers.%s.priority", t.routerName)
	t.Add(key, fmt.Sprintf("%d", priority))
	return t
}

// Service links the router to a specific service
func (t *TraefikLabelBuilder) Service(serviceName string) *TraefikLabelBuilder {
	key := fmt.Sprintf("traefik.http.routers.%s.service", t.routerName)
	t.Add(key, serviceName)
	return t
}

// Middlewares adds middleware(s) to the router
func (t *TraefikLabelBuilder) Middlewares(middlewares ...string) *TraefikLabelBuilder {
	key := fmt.Sprintf("traefik.http.routers.%s.middlewares", t.routerName)
	t.Add(key, strings.Join(middlewares, ","))
	return t
}

// RedirectRegexMiddleware creates a redirect regex middleware
// This is used to redirect one domain to another (e.g., www -> non-www)
func (t *TraefikLabelBuilder) RedirectRegexMiddleware(middlewareName, fromDomain, toDomain string, permanent bool) *TraefikLabelBuilder {
	// Escape dots in domain for regex
	escapedFromDomain := strings.ReplaceAll(fromDomain, ".", "\\.")

	// Regex to match the source domain and capture the path
	regex := fmt.Sprintf("^https?://%s/(.*)", escapedFromDomain)
	replacement := fmt.Sprintf("https://%s/${1}", toDomain)

	t.Add(fmt.Sprintf("traefik.http.middlewares.%s.redirectregex.regex", middlewareName), regex)
	t.Add(fmt.Sprintf("traefik.http.middlewares.%s.redirectregex.replacement", middlewareName), replacement)
	t.Add(fmt.Sprintf("traefik.http.middlewares.%s.redirectregex.permanent", middlewareName), fmt.Sprintf("%t", permanent))

	return t
}

// RedirectDomainBuilder helps build redirect configurations for domain redirects
type RedirectDomainBuilder struct {
	containerName string
	primaryDomain string
	labels        []string
}

// NewRedirectDomainBuilder creates a new redirect domain builder
func NewRedirectDomainBuilder(containerName, primaryDomain string) *RedirectDomainBuilder {
	return &RedirectDomainBuilder{
		containerName: containerName,
		primaryDomain: primaryDomain,
		labels:        make([]string, 0),
	}
}

// AddRedirectDomain adds a domain that should redirect to the primary domain
func (r *RedirectDomainBuilder) AddRedirectDomain(redirectDomain string, index int, certResolver string) *RedirectDomainBuilder {
	routerName := fmt.Sprintf("%s-redirect-%d", r.containerName, index)
	middlewareName := fmt.Sprintf("%s-redirect-%d", r.containerName, index)

	builder := NewTraefikLabelBuilder(routerName, r.containerName)

	// Router for the redirect domain
	builder.HostRule(redirectDomain)
	builder.Entrypoints("web", "websecure")

	// TLS for the redirect domain (it needs its own certificate)
	if certResolver != "" {
		builder.TLS(certResolver)
	}

	// Create redirect middleware
	builder.RedirectRegexMiddleware(middlewareName, redirectDomain, r.primaryDomain, true)

	// Apply the middleware to the router
	builder.Middlewares(middlewareName)

	// Add all labels
	r.labels = append(r.labels, builder.BuildSlice()...)

	return r
}

// Build returns all redirect labels as a space-separated string
func (r *RedirectDomainBuilder) Build() string {
	return strings.Join(r.labels, " ")
}

// BuildSlice returns all redirect labels as a slice
func (r *RedirectDomainBuilder) BuildSlice() []string {
	return r.labels
}
