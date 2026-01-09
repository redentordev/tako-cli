package dependency

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// Resolver resolves service dependencies and provides deployment order
type Resolver struct {
	services map[string]config.ServiceConfig
	verbose  bool
}

// NewResolver creates a new dependency resolver
func NewResolver(services map[string]config.ServiceConfig, verbose bool) *Resolver {
	return &Resolver{
		services: services,
		verbose:  verbose,
	}
}

// ResolveOrder returns services in dependency order using topological sort
// Returns error if circular dependencies are detected
func (r *Resolver) ResolveOrder() ([]string, error) {
	// Build dependency graph
	graph := make(map[string][]string)
	inDegree := make(map[string]int)

	// Initialize all services
	for name := range r.services {
		graph[name] = []string{}
		inDegree[name] = 0
	}

	// Build edges (dependencies)
	for name, service := range r.services {
		for _, dep := range service.DependsOn {
			// Validate dependency exists
			if _, exists := r.services[dep]; !exists {
				return nil, fmt.Errorf("service '%s' depends on '%s' which does not exist", name, dep)
			}

			graph[dep] = append(graph[dep], name)
			inDegree[name]++
		}
	}

	// Topological sort using Kahn's algorithm
	var result []string
	queue := []string{}

	// Find all services with no dependencies
	for name, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, name)
		}
	}

	// Sort queue for deterministic ordering
	// Map iteration order is random, but we want consistent deployment order
	sort.Strings(queue)

	// Process queue
	for len(queue) > 0 {
		// Get next service
		current := queue[0]
		queue = queue[1:]
		result = append(result, current)

		// Process dependents - collect newly ready services
		var newlyReady []string
		for _, dependent := range graph[current] {
			inDegree[dependent]--
			if inDegree[dependent] == 0 {
				newlyReady = append(newlyReady, dependent)
			}
		}

		// Sort and append newly ready services for deterministic order
		if len(newlyReady) > 0 {
			sort.Strings(newlyReady)
			queue = append(queue, newlyReady...)
		}
	}

	// Check for circular dependencies
	if len(result) != len(r.services) {
		// Find which services are in the cycle
		var cycleServices []string
		for name, degree := range inDegree {
			if degree > 0 {
				cycleServices = append(cycleServices, name)
			}
		}
		return nil, fmt.Errorf("circular dependency detected involving: %s", strings.Join(cycleServices, ", "))
	}

	if r.verbose {
		fmt.Printf("\n=== Deployment Order (based on dependencies) ===\n")
		for i, name := range result {
			service := r.services[name]
			if len(service.DependsOn) > 0 {
				fmt.Printf("  %d. %s (depends on: %s)\n", i+1, name, strings.Join(service.DependsOn, ", "))
			} else {
				fmt.Printf("  %d. %s (no dependencies)\n", i+1, name)
			}
		}
		fmt.Println()
	}

	return result, nil
}

// InferDependencies automatically infers dependencies based on environment variables
// Returns a map of service name to inferred dependencies
func (r *Resolver) InferDependencies() map[string][]string {
	inferred := make(map[string][]string)

	for name, service := range r.services {
		var deps []string

		// Check environment variables for references to other services
		for key, value := range service.Env {
			for otherName := range r.services {
				if otherName == name {
					continue // Skip self
				}

				// Common patterns that indicate dependencies:
				// - SERVICE_URL, SERVICE_HOST, SERVICE_PORT
				// - DATABASE_URL, DB_HOST, POSTGRES_HOST, etc.
				// - REDIS_URL, REDIS_HOST, etc.
				// - Connection strings with @servicename: (for DB URLs like user:pass@postgres:5432)
				// - Direct service name references in HOST/SERVER env vars (e.g., database__connection__host: mysql)

				// Get the environment variable key to check if it's a host/server reference
				upperOther := strings.ToUpper(otherName)
				upperValue := strings.ToUpper(value)
				lowerOther := strings.ToLower(otherName)
				lowerValue := strings.ToLower(value)

				// First check: does the value equal the service name exactly AND is it in a host/server variable?
				// This handles cases like: database__connection__host: mysql
				// But avoids false positives like: MYSQL_DATABASE: ghost (where ghost is the DB name, not a service)
				if lowerValue == lowerOther {
					// Only consider it a dependency if the key name suggests it's a host/server reference
					// Check for common host/server key patterns
					keyLower := strings.ToLower(key)
					isHostKey := strings.Contains(keyLower, "host") ||
						strings.Contains(keyLower, "server") ||
						strings.Contains(keyLower, "url") ||
						strings.Contains(keyLower, "endpoint") ||
						strings.Contains(keyLower, "address") ||
						strings.Contains(keyLower, "addr")

					if isHostKey {
						matched := false
						// Check if already in deps
						for _, d := range deps {
							if d == otherName {
								matched = true
								break
							}
						}
						if !matched {
							deps = append(deps, otherName)
						}
						continue
					}
				}

				patterns := []string{
					upperOther + "_URL",
					upperOther + "_HOST",
					upperOther + "_PORT",
					upperOther + ":",
					"://" + lowerOther,
					"@" + lowerOther + ":", // For database URLs like postgresql://user:pass@postgres:5432
					"@" + lowerOther + "/", // For database URLs like postgresql://user:pass@postgres/db
				}

				matched := false
				for _, pattern := range patterns {
					checkValue := upperValue
					if strings.HasPrefix(pattern, "://") || strings.HasPrefix(pattern, "@") {
						checkValue = lowerValue
					}

					if strings.Contains(checkValue, pattern) {
						matched = true
						break
					}
				}

				if matched {
					// Check if already in deps
					found := false
					for _, d := range deps {
						if d == otherName {
							found = true
							break
						}
					}
					if !found {
						deps = append(deps, otherName)
					}
				}
			}
		}

		if len(deps) > 0 {
			inferred[name] = deps
		}
	}

	if r.verbose && len(inferred) > 0 {
		fmt.Printf("\n=== Inferred Dependencies ===\n")
		for name, deps := range inferred {
			fmt.Printf("  %s → %s\n", name, strings.Join(deps, ", "))
		}
		fmt.Println()
	}

	return inferred
}

// MergeDependencies merges explicit and inferred dependencies
// Explicit dependencies take precedence
func (r *Resolver) MergeDependencies(inferred map[string][]string) {
	for name, service := range r.services {
		// If service already has explicit dependencies, skip
		if len(service.DependsOn) > 0 {
			continue
		}

		// Use inferred dependencies if available
		if deps, ok := inferred[name]; ok {
			service.DependsOn = deps
			r.services[name] = service
		}
	}
}

// ValidateOrder ensures the deployment order is valid
// This is useful for testing and validation
func ValidateOrder(services map[string]config.ServiceConfig, order []string) error {
	deployed := make(map[string]bool)

	for _, name := range order {
		service, exists := services[name]
		if !exists {
			return fmt.Errorf("service '%s' in order but not in services", name)
		}

		// Check all dependencies are already deployed
		for _, dep := range service.DependsOn {
			if !deployed[dep] {
				return fmt.Errorf("service '%s' depends on '%s' but '%s' is deployed later", name, dep, dep)
			}
		}

		deployed[name] = true
	}

	return nil
}
