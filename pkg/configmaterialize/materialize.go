package configmaterialize

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi"
)

const defaultExportedVersion = "exported"

// Warning describes lossy or incomplete reconstruction performed while building
// a config from replicated takod state.
type Warning struct {
	Code    string
	Message string
	Service string
	Server  string
}

// Options contains the remote takod documents and caller-supplied connection
// details used to reconstruct a config.Config.
type Options struct {
	Desired *takoapi.DesiredStateDocument
	Actual  *takoapi.ActualStateDocument
	History *takoapi.DeploymentHistoryDocument

	// Servers supplies connection details that takod desired state does not store.
	// Any desired target node missing here is synthesized as a placeholder and
	// returned with a warning.
	Servers map[string]config.ServerConfig

	// Validate runs config.ValidateConfig on the materialized result.
	Validate bool
}

// BuildConfig materializes a config.Config from takod desired, actual, and
// deployment history documents. Desired state is authoritative; actual state is
// used only to fill services that are absent from desired with image/replicas.
func BuildConfig(opts Options) (*config.Config, []Warning, error) {
	var warnings []Warning

	project := firstNonEmpty(desiredProject(opts.Desired), actualProject(opts.Actual), historyProject(opts.History))
	environment := firstNonEmpty(desiredEnvironment(opts.Desired), actualEnvironment(opts.Actual), historyEnvironment(opts.History))
	if environment == "" {
		environment = "production"
		warnings = append(warnings, Warning{Code: "default_environment", Message: "environment was not present in state; using production"})
	}

	cfg := &config.Config{
		Project: config.ProjectConfig{
			Name:    project,
			Version: latestHistoryVersion(opts.History),
		},
		Servers:      cloneServers(opts.Servers),
		Environments: map[string]config.EnvironmentConfig{},
	}
	if cfg.Project.Version == "" {
		cfg.Project.Version = defaultExportedVersion
	}

	targetNodes := environmentServers(opts.Desired, opts.Actual, cfg.Servers)
	for _, node := range targetNodes {
		if _, ok := cfg.Servers[node]; !ok {
			cfg.Servers[node] = config.ServerConfig{Host: node, User: "root"}
			warnings = append(warnings, Warning{
				Code:    "placeholder_server",
				Server:  node,
				Message: fmt.Sprintf("server %q was present in target nodes but no connection details were supplied; synthesized placeholder host/user", node),
			})
		}
	}

	services, serviceWarnings, err := materializeServices(opts.Desired, opts.Actual)
	if err != nil {
		return nil, warnings, err
	}
	warnings = append(warnings, serviceWarnings...)

	cfg.Environments[environment] = config.EnvironmentConfig{
		Servers:  targetNodes,
		Services: services,
	}

	if opts.Validate {
		if err := config.ValidateConfig(cfg); err != nil {
			return nil, warnings, err
		}
	}

	return cfg, warnings, nil
}

func materializeServices(desired *takoapi.DesiredStateDocument, actual *takoapi.ActualStateDocument) (map[string]config.ServiceConfig, []Warning, error) {
	services := map[string]config.ServiceConfig{}
	var warnings []Warning

	if desired != nil {
		for _, name := range sortedDesiredServiceNames(desired.Services) {
			serviceDoc := desired.Services[name]
			service, serviceWarnings, err := desiredServiceToConfig(name, serviceDoc)
			if err != nil {
				return nil, warnings, err
			}
			services[name] = service
			warnings = append(warnings, serviceWarnings...)
		}
	}

	if actual != nil {
		for _, name := range sortedActualServiceNames(actual.Services) {
			if _, exists := services[name]; exists {
				continue
			}
			actualService := actual.Services[name]
			services[name] = config.ServiceConfig{
				Image:    strings.TrimSpace(actualService.Image),
				Replicas: actualService.Replicas,
			}
			warnings = append(warnings, Warning{
				Code:    "actual_only_service",
				Service: name,
				Message: fmt.Sprintf("service %q was not present in desired state; reconstructed only image and replicas from actual state", name),
			})
		}
	}

	return services, warnings, nil
}

func desiredServiceToConfig(name string, service takoapi.DesiredServiceDocument) (config.ServiceConfig, []Warning, error) {
	var warnings []Warning
	out := config.ServiceConfig{
		Image:      strings.TrimSpace(service.Image),
		Build:      strings.TrimSpace(service.Build),
		Command:    strings.TrimSpace(service.Command),
		Port:       service.Port,
		Replicas:   service.Replicas,
		Restart:    strings.TrimSpace(service.Restart),
		Persistent: service.Persistent,
		Volumes:    cleanStrings(service.Volumes),
		Secrets:    cleanStrings(service.SecretRefs),
		DependsOn:  cleanStrings(service.DependsOn),
	}

	if len(service.EnvKeys) > 0 {
		out.Env = map[string]string{}
		for _, key := range sortedStrings(service.EnvKeys) {
			key = strings.TrimSpace(key)
			if key != "" {
				out.Env[key] = ""
			}
		}
	}

	if len(service.Domains) > 0 {
		domains := cleanStrings(service.Domains)
		if len(domains) > 0 {
			out.Proxy = &config.ProxyConfig{Domain: domains[0]}
			if len(domains) > 1 {
				out.Proxy.RedirectFrom = append([]string(nil), domains[1:]...)
				warnings = append(warnings, Warning{
					Code:    "extra_domains_as_redirects",
					Service: name,
					Message: fmt.Sprintf("service %q had multiple domains; using %q as primary and remaining domains as redirectFrom", name, domains[0]),
				})
			}
		}
	}

	if strings.TrimSpace(service.DeployStrategy) != "" {
		out.Deploy.Strategy = strings.TrimSpace(service.DeployStrategy)
	}

	if len(service.Placement) > 0 {
		placement, err := decodeRaw[config.PlacementConfig](service.Placement, "placement", name)
		if err != nil {
			return config.ServiceConfig{}, warnings, err
		}
		out.Placement = &placement
	}

	if len(service.HealthCheck) > 0 {
		healthCheck, err := decodeRaw[config.HealthCheckConfig](service.HealthCheck, "healthCheck", name)
		if err != nil {
			return config.ServiceConfig{}, warnings, err
		}
		out.HealthCheck = healthCheck
	}

	return out, warnings, nil
}

func decodeRaw[T any](raw json.RawMessage, field string, service string) (T, error) {
	var out T
	if len(raw) == 0 || string(raw) == "null" {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("service %s: invalid %s: %w", service, field, err)
	}
	return out, nil
}

func environmentServers(desired *takoapi.DesiredStateDocument, actual *takoapi.ActualStateDocument, servers map[string]config.ServerConfig) []string {
	if desired != nil && len(desired.TargetNodes) > 0 {
		return cleanStrings(desired.TargetNodes)
	}
	if actual != nil && len(actual.TargetNodes) > 0 {
		return cleanStrings(actual.TargetNodes)
	}
	out := make([]string, 0, len(servers))
	for name := range servers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func latestHistoryVersion(history *takoapi.DeploymentHistoryDocument) string {
	if history == nil || len(history.Deployments) == 0 {
		return ""
	}
	latestIdx := -1
	for i, deployment := range history.Deployments {
		if deployment == nil || strings.TrimSpace(deployment.Version) == "" {
			continue
		}
		if latestIdx == -1 || deployment.Timestamp.After(history.Deployments[latestIdx].Timestamp) {
			latestIdx = i
		}
	}
	if latestIdx == -1 {
		return ""
	}
	return strings.TrimSpace(history.Deployments[latestIdx].Version)
}

func desiredProject(desired *takoapi.DesiredStateDocument) string {
	if desired == nil {
		return ""
	}
	return strings.TrimSpace(desired.Project)
}

func actualProject(actual *takoapi.ActualStateDocument) string {
	if actual == nil {
		return ""
	}
	return strings.TrimSpace(actual.Project)
}

func historyProject(history *takoapi.DeploymentHistoryDocument) string {
	if history == nil {
		return ""
	}
	return strings.TrimSpace(history.ProjectName)
}

func desiredEnvironment(desired *takoapi.DesiredStateDocument) string {
	if desired == nil {
		return ""
	}
	return strings.TrimSpace(desired.Environment)
}

func actualEnvironment(actual *takoapi.ActualStateDocument) string {
	if actual == nil {
		return ""
	}
	return strings.TrimSpace(actual.Environment)
}

func historyEnvironment(history *takoapi.DeploymentHistoryDocument) string {
	if history == nil {
		return ""
	}
	return strings.TrimSpace(history.Environment)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneServers(in map[string]config.ServerConfig) map[string]config.ServerConfig {
	out := make(map[string]config.ServerConfig, len(in))
	for name, server := range in {
		out[name] = server
	}
	return out
}

func sortedDesiredServiceNames(services map[string]takoapi.DesiredServiceDocument) []string {
	out := make([]string, 0, len(services))
	for name := range services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedActualServiceNames(services map[string]takoapi.ActualServiceDocument) []string {
	out := make([]string, 0, len(services))
	for name := range services {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(values []string) []string {
	out := cleanStrings(values)
	sort.Strings(out)
	return out
}

func cleanStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
