package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
)

type domainStatusSpec struct {
	Service string
	Domain  string
	Role    string
}

type domainStatusOptions struct {
	Timeout         time.Duration
	Strict          bool
	ExpectedTargets []string
}

func collectConfiguredDomainSpecs(services map[string]config.ServiceConfig, serviceFilter string) []domainStatusSpec {
	serviceNames := sortedServiceNames(services)
	var specs []domainStatusSpec
	for _, serviceName := range serviceNames {
		if serviceFilter != "" && serviceName != serviceFilter {
			continue
		}
		service := services[serviceName]
		if service.Proxy == nil || !service.IsPublic() {
			continue
		}
		for _, domain := range service.Proxy.GetAllDomains() {
			specs = append(specs, domainStatusSpec{Service: serviceName, Domain: domain, Role: "serving"})
		}
		for _, domain := range service.Proxy.GetRedirectDomains() {
			specs = append(specs, domainStatusSpec{Service: serviceName, Domain: domain, Role: "redirect"})
		}
	}
	return specs
}

func domainExpectedTargets(cfg *config.Config, envName string, overrides []string) ([]string, error) {
	if len(overrides) > 0 {
		return append([]string(nil), overrides...), nil
	}
	proxyServers, err := cfg.GetEnvironmentProxyServers(envName)
	if err != nil {
		return nil, err
	}
	targets := make([]string, 0, len(proxyServers))
	for _, serverName := range proxyServers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			return nil, fmt.Errorf("proxy server %s is not defined in servers", serverName)
		}
		targets = append(targets, server.Host)
	}
	return targets, nil
}

func monitorDomainStatuses(ctx context.Context, checker *health.HealthChecker, specs []domainStatusSpec, options domainStatusOptions) ([]health.DomainStatus, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	fmt.Println("\n🌐 Checking public domain DNS/TLS status...")
	if len(options.ExpectedTargets) > 0 {
		fmt.Printf("   Expected proxy target(s): %s\n", strings.Join(options.ExpectedTargets, ", "))
	}

	start := time.Now()
	attempt := 1
	var latest []health.DomainStatus
	check := func() []health.DomainStatus {
		results := make([]health.DomainStatus, 0, len(specs))
		allReady := true
		for _, spec := range specs {
			status := checker.CheckDomain(ctx, spec.Service, spec.Domain, options.ExpectedTargets)
			results = append(results, status)
			if status.Pending() {
				allReady = false
			}
		}
		printDomainStatusAttempt(attempt, results)
		attempt++
		if allReady {
			fmt.Printf("   ✓ All public domains active in %s\n", time.Since(start).Round(time.Second))
		}
		return results
	}

	latest = check()
	if allDomainStatusesReady(latest) || options.Timeout <= 0 {
		return latest, domainStatusStrictError(latest, options.Strict)
	}

	deadline := time.NewTimer(options.Timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return latest, ctx.Err()
		case <-deadline.C:
			printDomainPendingSummary(latest)
			return latest, domainStatusStrictError(latest, options.Strict)
		case <-ticker.C:
			latest = check()
			if allDomainStatusesReady(latest) {
				return latest, nil
			}
		}
	}
}

func printDomainStatusAttempt(attempt int, statuses []health.DomainStatus) {
	for _, status := range statuses {
		fmt.Printf("   [%d] %s (%s): %s", attempt, status.Domain, status.Service, status.State)
		if status.Message != "" {
			fmt.Printf(" - %s", status.Message)
		}
		if len(status.ResolvedIPs) > 0 {
			fmt.Printf(" [resolved: %s]", strings.Join(status.ResolvedIPs, ", "))
		}
		if status.CNAME != "" {
			fmt.Printf(" [cname: %s]", status.CNAME)
		}
		fmt.Println()
	}
}

func printDomainPendingSummary(statuses []health.DomainStatus) {
	pending := pendingDomainStatuses(statuses)
	if len(pending) == 0 {
		return
	}
	fmt.Println("\n⚠️  Public domain provisioning is still pending.")
	for _, status := range pending {
		fmt.Printf("   - %s (%s): %s", status.Domain, status.Service, status.State)
		if status.Message != "" {
			fmt.Printf(" - %s", status.Message)
		}
		fmt.Println()
	}
	fmt.Println("   Deployment already reconciled. Fix DNS or wait for propagation, then run `tako domains status`.")
}

func allDomainStatusesReady(statuses []health.DomainStatus) bool {
	for _, status := range statuses {
		if status.Pending() {
			return false
		}
	}
	return true
}

func pendingDomainStatuses(statuses []health.DomainStatus) []health.DomainStatus {
	var pending []health.DomainStatus
	for _, status := range statuses {
		if status.Pending() {
			pending = append(pending, status)
		}
	}
	return pending
}

func domainStatusStrictError(statuses []health.DomainStatus, strict bool) error {
	if !strict {
		return nil
	}
	pending := pendingDomainStatuses(statuses)
	if len(pending) == 0 {
		return nil
	}
	parts := make([]string, 0, len(pending))
	for _, status := range pending {
		parts = append(parts, fmt.Sprintf("%s=%s", status.Domain, status.State))
	}
	return fmt.Errorf("public domain check failed in strict mode: %s", strings.Join(parts, ", "))
}
