package reconcile

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// ChangeType represents the type of change needed
type ChangeType string

const (
	ChangeAdd    ChangeType = "add"    // New service in config
	ChangeUpdate ChangeType = "update" // Service exists but changed
	ChangeRemove ChangeType = "remove" // Service removed from config
	ChangeNone   ChangeType = "none"   // No changes needed
)

// ServiceChange represents a change to be made to a service
type ServiceChange struct {
	Type        ChangeType
	ServiceName string
	OldConfig   *config.ServiceConfig // nil for adds
	NewConfig   *config.ServiceConfig // nil for removes
	Reasons     []string              // Why this change is needed
}

// ReconciliationPlan represents the full plan of changes
type ReconciliationPlan struct {
	ProjectName string
	Environment string
	Changes     []ServiceChange
	Summary     ReconciliationSummary
}

// ReconciliationSummary provides counts of changes
type ReconciliationSummary struct {
	Total   int
	Adds    int
	Updates int
	Removes int
	NoOps   int
}

// ComputePlan compares desired state (config) with actual state (running services)
// and generates a reconciliation plan
func ComputePlan(
	projectName string,
	environment string,
	desiredServices map[string]config.ServiceConfig, // From tako.yaml
	actualServices map[string]*ActualService, // Currently running
) *ReconciliationPlan {

	plan := &ReconciliationPlan{
		ProjectName: projectName,
		Environment: environment,
		Changes:     []ServiceChange{},
	}

	// Track which actual services we've matched
	matchedActual := make(map[string]bool)

	// 1. Check all services in config (desired state)
	for serviceName, desiredConfig := range desiredServices {
		actual, exists := actualServices[serviceName]

		if !exists {
			// Service in config but not running -> ADD
			change := ServiceChange{
				Type:        ChangeAdd,
				ServiceName: serviceName,
				NewConfig:   &desiredConfig,
				Reasons:     []string{"Service defined in config but not deployed"},
			}
			plan.Changes = append(plan.Changes, change)
			plan.Summary.Adds++
		} else {
			// Service exists -> check if UPDATE needed
			matchedActual[serviceName] = true
			reasons := detectChanges(desiredConfig, actual)

			if len(reasons) > 0 {
				// Changes detected -> UPDATE
				oldConfig := actual.ConfigSnapshot
				change := ServiceChange{
					Type:        ChangeUpdate,
					ServiceName: serviceName,
					OldConfig:   oldConfig,
					NewConfig:   &desiredConfig,
					Reasons:     reasons,
				}
				plan.Changes = append(plan.Changes, change)
				plan.Summary.Updates++
			} else {
				// No changes -> NO-OP
				change := ServiceChange{
					Type:        ChangeNone,
					ServiceName: serviceName,
					OldConfig:   actual.ConfigSnapshot,
					NewConfig:   &desiredConfig,
					Reasons:     []string{"Service is up-to-date"},
				}
				plan.Changes = append(plan.Changes, change)
				plan.Summary.NoOps++
			}
		}
	}

	// 2. Check for services running but not in config (should be removed)
	for serviceName, actual := range actualServices {
		if !matchedActual[serviceName] {
			// Check if service is marked as persistent (don't auto-remove)
			isPersistent := false
			if actual.ConfigSnapshot != nil && actual.ConfigSnapshot.Persistent {
				isPersistent = true
			}

			if isPersistent {
				// Persistent service (database, etc.) - don't remove automatically
				change := ServiceChange{
					Type:        ChangeNone,
					ServiceName: serviceName,
					OldConfig:   actual.ConfigSnapshot,
					Reasons: []string{
						"Service removed from config but marked as PERSISTENT",
						"Keeping service running (databases, stateful services)",
						"To remove, use: tako stop " + serviceName,
					},
				}
				plan.Changes = append(plan.Changes, change)
				plan.Summary.NoOps++
			} else {
				// Non-persistent service running but not in config -> REMOVE
				change := ServiceChange{
					Type:        ChangeRemove,
					ServiceName: serviceName,
					OldConfig:   actual.ConfigSnapshot,
					Reasons: []string{
						"Service is running but no longer defined in tako.yaml",
						"Will stop and remove containers/services",
					},
				}
				plan.Changes = append(plan.Changes, change)
				plan.Summary.Removes++
			}
		}
	}

	plan.Summary.Total = len(plan.Changes)

	return plan
}

// ActualService represents a currently running service
type ActualService struct {
	Name           string
	Image          string
	Replicas       int
	Containers     []string
	ConfigSnapshot *config.ServiceConfig // Last deployed config
}

// detectChanges compares config with actual service and returns reasons for update
func detectChanges(desired config.ServiceConfig, actual *ActualService) []string {
	reasons := []string{}

	// Safety check
	if actual == nil || actual.ConfigSnapshot == nil {
		reasons = append(reasons, "Service configuration changed (no previous state)")
		return reasons
	}

	oldConfig := actual.ConfigSnapshot

	// Compare image (if specified)
	if desired.Image != "" && desired.Image != actual.Image {
		reasons = append(reasons, fmt.Sprintf("Image changed: %s → %s", actual.Image, desired.Image))
	}

	// Compare replicas
	desiredReplicas := desired.Replicas
	if desiredReplicas == 0 {
		desiredReplicas = 1
	}
	if desiredReplicas != actual.Replicas {
		reasons = append(reasons, fmt.Sprintf("Replicas changed: %d → %d", actual.Replicas, desiredReplicas))
	}

	// Compare port (only if both are set)
	if desired.Port > 0 && oldConfig.Port > 0 && desired.Port != oldConfig.Port {
		reasons = append(reasons, fmt.Sprintf("Port changed: %d → %d", oldConfig.Port, desired.Port))
	}

	// Compare environment variables
	if !envMapsEqual(desired.Env, oldConfig.Env) {
		reasons = append(reasons, "Environment variables changed")
	}

	// Compare build context (triggers rebuild)
	if desired.Build != "" && oldConfig.Build != "" && desired.Build != oldConfig.Build {
		reasons = append(reasons, "Build context changed - rebuild required")
	}

	// Compare domain configuration
	if !domainsEqual(desired.Proxy, oldConfig.Proxy) {
		reasons = append(reasons, "Domain/proxy configuration changed")
	}

	// Compare volume mounts
	if !volumesEqual(desired.Volumes, oldConfig.Volumes) {
		reasons = append(reasons, "Volume configuration changed")
	}

	return reasons
}

// Helper functions for comparing configurations

func envMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func domainsEqual(a, b *config.ProxyConfig) bool {
	// Both nil
	if a == nil && b == nil {
		return true
	}
	// One nil, one not
	if (a == nil) != (b == nil) {
		return false
	}
	// Compare domains
	if len(a.Domains) != len(b.Domains) {
		return false
	}
	for i := range a.Domains {
		if a.Domains[i] != b.Domains[i] {
			return false
		}
	}
	return true
}

func volumesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FormatPlan returns a human-readable formatted plan
func (p *ReconciliationPlan) FormatPlan() string {
	var sb strings.Builder

	sb.WriteString("═══════════════════════════════════════════\n")
	sb.WriteString(fmt.Sprintf("  DEPLOYMENT PLAN: %s (%s)\n", p.ProjectName, p.Environment))
	sb.WriteString("═══════════════════════════════════════════\n\n")

	if p.Summary.Total == 0 || (p.Summary.Total == p.Summary.NoOps) {
		sb.WriteString("✓ No changes needed - all services up-to-date\n\n")
		sb.WriteString("═══════════════════════════════════════════\n")
		return sb.String()
	}

	sb.WriteString(fmt.Sprintf("Summary: %d change(s)\n", p.Summary.Total-p.Summary.NoOps))
	if p.Summary.Adds > 0 {
		sb.WriteString(fmt.Sprintf("  + %d service(s) to add\n", p.Summary.Adds))
	}
	if p.Summary.Updates > 0 {
		sb.WriteString(fmt.Sprintf("  ↻ %d service(s) to update\n", p.Summary.Updates))
	}
	if p.Summary.Removes > 0 {
		sb.WriteString(fmt.Sprintf("  - %d service(s) to remove\n", p.Summary.Removes))
	}
	if p.Summary.NoOps > 0 {
		sb.WriteString(fmt.Sprintf("  ✓ %d service(s) unchanged\n", p.Summary.NoOps))
	}
	sb.WriteString("\n")

	// Details
	for _, change := range p.Changes {
		switch change.Type {
		case ChangeAdd:
			sb.WriteString(fmt.Sprintf("+ ADD: %s\n", change.ServiceName))
			for _, reason := range change.Reasons {
				sb.WriteString(fmt.Sprintf("    → %s\n", reason))
			}
			sb.WriteString("\n")

		case ChangeUpdate:
			sb.WriteString(fmt.Sprintf("↻ UPDATE: %s\n", change.ServiceName))
			for _, reason := range change.Reasons {
				sb.WriteString(fmt.Sprintf("    → %s\n", reason))
			}
			sb.WriteString("\n")

		case ChangeRemove:
			sb.WriteString(fmt.Sprintf("- REMOVE: %s\n", change.ServiceName))
			for _, reason := range change.Reasons {
				sb.WriteString(fmt.Sprintf("    → %s\n", reason))
			}
			sb.WriteString("\n")

		case ChangeNone:
			// Skip no-ops in detailed view unless verbose
			// sb.WriteString(fmt.Sprintf("✓ UNCHANGED: %s\n", change.ServiceName))
		}
	}

	sb.WriteString("═══════════════════════════════════════════\n")

	return sb.String()
}

// HasDestructiveChanges returns true if plan includes removals
func (p *ReconciliationPlan) HasDestructiveChanges() bool {
	return p.Summary.Removes > 0
}

// NeedsConfirmation returns true if user should confirm before proceeding
func (p *ReconciliationPlan) NeedsConfirmation() bool {
	return p.HasDestructiveChanges() || p.Summary.Updates > 0
}

// IsEmpty returns true if no changes needed
func (p *ReconciliationPlan) IsEmpty() bool {
	return p.Summary.Total == p.Summary.NoOps
}
