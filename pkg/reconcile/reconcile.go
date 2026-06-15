package reconcile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
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
			reasons := detectChanges(projectName, environment, serviceName, desiredConfig, actual)

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
						"To stop replicas, use: tako scale " + serviceName + "=0",
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
	Name                  string
	Image                 string
	Replicas              int
	Containers            []string
	ConfigHash            string
	RuntimeID             string
	HealthyReplicas       int
	UnhealthyReplicas     int
	StartingReplicas      int
	NoHealthcheckReplicas int
	UnknownHealthReplicas int
	ConfigSnapshot        *config.ServiceConfig // Last deployed config
}

// detectChanges compares config with actual service and returns reasons for update
func detectChanges(projectName string, environment string, serviceName string, desired config.ServiceConfig, actual *ActualService) []string {
	reasons := []string{}

	// Safety check
	if actual == nil || actual.ConfigSnapshot == nil {
		reasons = append(reasons, "Service configuration changed (no previous state)")
		return reasons
	}

	oldConfig := actual.ConfigSnapshot

	if projectName != "" && environment != "" && serviceName != "" {
		expectedRuntimeID := runtimeid.ServiceIdentity(projectName, environment, serviceName)
		if actual.RuntimeID != expectedRuntimeID {
			reasons = append(reasons, "Runtime identity changed")
		}
	}

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
	if len(reasons) > 0 {
		return reasons
	}

	if actual.ConfigHash != "" {
		if desiredHash, ok := SafeServiceConfigHash(desired); ok {
			if actual.ConfigHash == desiredHash {
				return nil
			}
			return []string{"Service configuration changed"}
		}
	}

	if !portsEqual(desired, *oldConfig) {
		reasons = append(reasons, "Port configuration changed")
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
	if !serviceDomainsEqual(desired, *oldConfig) {
		reasons = append(reasons, "Domain/proxy configuration changed")
	}

	// Compare volume mounts
	if !volumesEqual(desired.Volumes, oldConfig.Volumes) {
		reasons = append(reasons, "Volume configuration changed")
	}

	if !configsEqual(desired.Configs, oldConfig.Configs) {
		reasons = append(reasons, "Config file configuration changed")
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
	if a == nil && b == nil {
		return true
	}
	if (a == nil) != (b == nil) {
		return false
	}
	return a.Domain == b.Domain && stringSlicesEqual(a.RedirectFrom, b.RedirectFrom)
}

func portsEqual(a, b config.ServiceConfig) bool {
	return portSlicesEqual(a.EffectivePorts(), b.EffectivePorts())
}

func portSlicesEqual(a, b []config.PortConfig) bool {
	if len(a) != len(b) {
		return false
	}
	sort.SliceStable(a, func(i, j int) bool {
		if a[i].Name == a[j].Name {
			return a[i].Target < a[j].Target
		}
		return a[i].Name < a[j].Name
	})
	sort.SliceStable(b, func(i, j int) bool {
		if b[i].Name == b[j].Name {
			return b[i].Target < b[j].Target
		}
		return b[i].Name < b[j].Name
	})
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].Target != b[i].Target ||
			a[i].Published != b[i].Published ||
			a[i].Protocol != b[i].Protocol ||
			a[i].Mode != b[i].Mode ||
			a[i].HostIP != b[i].HostIP ||
			a[i].Internal != b[i].Internal ||
			!domainsEqual(a[i].Proxy, b[i].Proxy) {
			return false
		}
	}
	return true
}

func serviceDomainsEqual(a, b config.ServiceConfig) bool {
	return stringSlicesEqual(serviceDomains(a), serviceDomains(b))
}

func serviceDomains(service config.ServiceConfig) []string {
	var domains []string
	for _, port := range service.EffectivePorts() {
		if port.Proxy == nil {
			continue
		}
		domains = append(domains, port.Proxy.GetAllDomains()...)
		domains = append(domains, port.Proxy.GetRedirectDomains()...)
	}
	sort.Strings(domains)
	return domains
}

func stringSlicesEqual(a, b []string) bool {
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

func volumesEqual(a, b []string) bool {
	return stringSlicesEqual(a, b)
}

func configsEqual(a, b []config.ServiceConfigFileMount) bool {
	a = sortedConfigFilesForCompare(a)
	b = sortedConfigFilesForCompare(b)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Source != b[i].Source ||
			a[i].Target != b[i].Target ||
			a[i].Mode != b[i].Mode ||
			a[i].ContentHash != b[i].ContentHash {
			return false
		}
	}
	return true
}

func sortedConfigFilesForCompare(configs []config.ServiceConfigFileMount) []config.ServiceConfigFileMount {
	if len(configs) == 0 {
		return nil
	}
	out := append([]config.ServiceConfigFileMount(nil), configs...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source == out[j].Source {
			return out[i].Target < out[j].Target
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// FormatPlan returns a human-readable formatted plan
func (p *ReconciliationPlan) FormatPlan() string {
	return p.FormatPlanVerbose(false)
}

// FormatPlanVerbose returns a detailed formatted plan with optional unchanged services
func (p *ReconciliationPlan) FormatPlanVerbose(showUnchanged bool) string {
	var sb strings.Builder

	sb.WriteString("┌─────────────────────────────────────────────────────────────┐\n")
	sb.WriteString(fmt.Sprintf("│  Deployment Plan: %s (%s)\n", p.ProjectName, p.Environment))
	sb.WriteString("└─────────────────────────────────────────────────────────────┘\n\n")

	if p.Summary.Total == 0 || (p.Summary.Total == p.Summary.NoOps) {
		sb.WriteString("  ✓ All services are up-to-date. No changes needed.\n\n")
		return sb.String()
	}

	// Summary table
	changeCount := p.Summary.Total - p.Summary.NoOps
	sb.WriteString(fmt.Sprintf("  %d change(s) to apply:\n\n", changeCount))

	// Table header
	sb.WriteString("  ┌────────────────┬────────────┬─────────────────────────────────────┐\n")
	sb.WriteString("  │ Service        │ Action     │ Details                             │\n")
	sb.WriteString("  ├────────────────┼────────────┼─────────────────────────────────────┤\n")

	// Table rows
	for _, change := range p.Changes {
		if change.Type == ChangeNone && !showUnchanged {
			continue
		}

		serviceName := truncateString(change.ServiceName, 14)
		action := ""
		details := ""

		switch change.Type {
		case ChangeAdd:
			action = "+ CREATE"
			if change.NewConfig != nil {
				if change.NewConfig.Image != "" {
					details = fmt.Sprintf("image: %s", truncateString(change.NewConfig.Image, 30))
				} else if change.NewConfig.Build != "" {
					details = fmt.Sprintf("build: %s", truncateString(change.NewConfig.Build, 30))
				}
			}
		case ChangeUpdate:
			action = "~ UPDATE"
			if len(change.Reasons) > 0 {
				details = truncateString(change.Reasons[0], 35)
			}
		case ChangeRemove:
			action = "- REMOVE"
			details = "Will stop and remove"
		case ChangeNone:
			action = "  (ok)"
			details = "No changes"
		}

		sb.WriteString(fmt.Sprintf("  │ %-14s │ %-10s │ %-35s │\n", serviceName, action, details))
	}

	sb.WriteString("  └────────────────┴────────────┴─────────────────────────────────────┘\n\n")

	// Detailed changes
	hasDetails := false
	for _, change := range p.Changes {
		if change.Type == ChangeNone {
			continue
		}
		if !hasDetails {
			sb.WriteString("  Details:\n\n")
			hasDetails = true
		}

		switch change.Type {
		case ChangeAdd:
			sb.WriteString(fmt.Sprintf("  + %s (CREATE)\n", change.ServiceName))
			if change.NewConfig != nil {
				if change.NewConfig.Image != "" {
					sb.WriteString(fmt.Sprintf("      Image: %s\n", change.NewConfig.Image))
				} else if change.NewConfig.Build != "" {
					sb.WriteString(fmt.Sprintf("      Build: %s\n", change.NewConfig.Build))
				}
				if change.NewConfig.Port > 0 {
					sb.WriteString(fmt.Sprintf("      Port: %d\n", change.NewConfig.Port))
				}
				replicas := change.NewConfig.Replicas
				if replicas == 0 {
					replicas = 1
				}
				sb.WriteString(fmt.Sprintf("      Replicas: %d\n", replicas))
				if change.NewConfig.Proxy != nil {
					domains := change.NewConfig.Proxy.GetAllDomains()
					if len(domains) > 0 {
						sb.WriteString(fmt.Sprintf("      Domains: %s\n", strings.Join(domains, ", ")))
					}
				}
			}
			sb.WriteString("\n")

		case ChangeUpdate:
			sb.WriteString(fmt.Sprintf("  ~ %s (UPDATE)\n", change.ServiceName))
			for _, reason := range change.Reasons {
				sb.WriteString(fmt.Sprintf("      - %s\n", reason))
			}
			sb.WriteString("\n")

		case ChangeRemove:
			sb.WriteString(fmt.Sprintf("  - %s (REMOVE)\n", change.ServiceName))
			sb.WriteString("      Service will be stopped and containers removed\n")
			if change.OldConfig != nil && change.OldConfig.Persistent {
				sb.WriteString("      WARNING: Service was marked as persistent!\n")
			}
			sb.WriteString("\n")
		}
	}

	// Warning for destructive changes
	if p.HasDestructiveChanges() {
		sb.WriteString("  ⚠ WARNING: This plan includes service removals.\n")
		sb.WriteString("    Removed services will be stopped and their containers deleted.\n")
		sb.WriteString("    Volumes marked for removal will be permanently deleted.\n\n")
	}

	return sb.String()
}

// truncateString truncates a string to maxLen and adds "..." if needed
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
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
