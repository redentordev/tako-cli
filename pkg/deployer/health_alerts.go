package deployer

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/alerttarget"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// healthAlertPlan is the node-side monitoring part of an alert request and
// whether it came from explicit keys. Explicit sections require
// monitoring.health-v1 on every schedulable target (deploy exits 2
// otherwise); default-derived sections are sent only to capable nodes.
type healthAlertPlan struct {
	Monitors       []takod.MonitorSpec
	Host           *takod.HostAlertPolicy
	Containers     *takod.ContainerAlertPolicy
	Certificates   *takod.CertificateAlertPolicy
	ServiceTargets map[string]string
	Explicit       bool
	Warnings       []string
}

func (p healthAlertPlan) enabled() bool {
	return len(p.Monitors) > 0 || p.Host != nil || p.Containers != nil || p.Certificates != nil
}

func (p healthAlertPlan) apply(request takod.AlertConfigRequest) takod.AlertConfigRequest {
	request.Monitors = p.Monitors
	request.Host = p.Host
	request.Containers = p.Containers
	request.Certificates = p.Certificates
	request.ServiceTargets = p.ServiceTargets
	return request
}

func withoutHealthSections(request takod.AlertConfigRequest) takod.AlertConfigRequest {
	request.Monitors, request.Host, request.Containers, request.Certificates, request.ServiceTargets = nil, nil, nil, nil, nil
	return request
}

func deliverableTarget(provider, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	normalized, err := alerttarget.NormalizeAndValidate(provider, value)
	if err != nil {
		return ""
	}
	return normalized
}

// buildHealthAlertPlan derives monitors and host/container/certificate
// sections from the config. checker picks the http checker node.
func (d *Deployer) buildHealthAlertPlan(targets takod.AlertTargets, checker string) (healthAlertPlan, error) {
	plan := healthAlertPlan{ServiceTargets: map[string]string{}}
	var alerts *config.SchedulerAlertsConfig
	if d.config.Notifications != nil {
		alerts = d.config.Notifications.Alerts
	}
	hasShared := targets.Slack != "" || targets.Discord != "" || targets.Webhook != ""
	if alerts != nil {
		plan.Explicit = alerts.ExplicitHealthSections()
		if hasShared {
			host := alerts.Host
			plan.Host = &takod.HostAlertPolicy{
				DiskPercent: host.HostDiskThreshold(), InodesPercent: host.HostInodesThreshold(),
				CPUForSeconds: int64(host.HostCPUForDuration().Seconds()), Conditions: host.HostConditionsEnabled(),
			}
			if host != nil {
				plan.Host.MemoryPercent, plan.Host.CPUPercent, plan.Host.LoadPerCore = host.Memory, host.CPU, host.Load
			}
			certificates := alerts.Certificates
			warn, critical := certificates.CertificateThresholds()
			plan.Certificates = &takod.CertificateAlertPolicy{WarnBeforeSeconds: int64(warn.Seconds()), CriticalBeforeSeconds: int64(critical.Seconds())}
		}
		containers := alerts.Containers
		plan.Containers = &takod.ContainerAlertPolicy{
			OOM: containers.ContainerOOMEnabled(), CrashLoopRestarts: containers.ContainerCrashLoopThreshold(),
			CrashLoopWindowSeconds: int64(containers.CrashLoopWindowDuration().Seconds()),
		}
	}
	services, err := d.config.GetServices(d.environment)
	if err != nil {
		return healthAlertPlan{}, err
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		service := services[name]
		monitoring := service.Monitoring
		if monitoring == nil || !monitoring.Enabled || service.IsJob() || service.IsRun() {
			continue
		}
		if monitoring.ExplicitHealthKeys() {
			plan.Explicit = true
		}
		serviceTarget := monitoring.DeliverableWebhook()
		if monitoring.WebhookDeprecated() {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("service %s: monitoring.webhook is not a public HTTPS alert target and is ignored", name))
		}
		if serviceTarget == "" && !hasShared {
			continue
		}
		interval, timeout := config.MonitorTiming(monitoring)
		alertAfter, recoverAfter := config.MonitorDamping(alerts, monitoring)
		spec := takod.MonitorSpec{
			Service: name, CheckType: takod.MonitorCheckContainer,
			IntervalSeconds: int64(interval.Seconds()), TimeoutSeconds: int64(timeout.Seconds()),
			AlertAfter: alertAfter, RecoverAfter: recoverAfter,
		}
		if spec.TimeoutSeconds < 1 {
			spec.TimeoutSeconds = 1
		}
		if monitoring.CheckType == takod.MonitorCheckHTTP && service.Proxy != nil {
			domain := strings.ToLower(strings.TrimSpace(service.Proxy.GetPrimaryDomain()))
			if domain != "" && !strings.ContainsAny(domain, "*{}$ ") {
				path := strings.TrimSpace(service.HealthCheck.Path)
				if path == "" || !strings.HasPrefix(path, "/") {
					path = "/"
				}
				spec.CheckType, spec.Domain, spec.HealthPath, spec.CheckerNode = takod.MonitorCheckHTTP, domain, path, checker
				spec.BasicAuth = service.Proxy.BasicAuth != nil
			}
		}
		if spec.CheckType != monitoring.CheckType && monitoring.CheckType == takod.MonitorCheckHTTP {
			plan.Warnings = append(plan.Warnings, fmt.Sprintf("service %s: no routable domain for an http check; a container check is used", name))
		}
		plan.Monitors = append(plan.Monitors, spec)
		if serviceTarget != "" {
			plan.ServiceTargets[name] = serviceTarget
		}
	}
	if len(plan.ServiceTargets) == 0 {
		plan.ServiceTargets = nil
	}
	if !hasShared && len(plan.ServiceTargets) == 0 {
		plan.Host, plan.Containers, plan.Certificates = nil, nil, nil
	}
	return plan, nil
}

// healthCheckerNode is the node that runs http checks: the first sorted
// schedulable server the deploy writes proxy routes to, else the first
// schedulable target.
func (d *Deployer) healthCheckerNode(schedulable []string) string {
	allowed := make(map[string]bool, len(schedulable))
	for _, server := range schedulable {
		allowed[server] = true
	}
	if proxyServers, err := d.getTakodProxyTargetServers(); err == nil {
		sorted := append([]string(nil), proxyServers...)
		sort.Strings(sorted)
		for _, server := range sorted {
			if allowed[server] {
				return server
			}
		}
	}
	sorted := append([]string(nil), schedulable...)
	sort.Strings(sorted)
	if len(sorted) > 0 {
		return sorted[0]
	}
	return ""
}

// fullAlertRequest builds the request with node-side monitoring sections.
func (d *Deployer) fullAlertRequest() (takod.AlertConfigRequest, healthAlertPlan, error) {
	request, err := d.buildAlertConfigRequest()
	if err != nil {
		return request, healthAlertPlan{}, err
	}
	schedulable, err := d.alertTargetServers(true)
	if err != nil {
		return request, healthAlertPlan{}, fmt.Errorf("resolve scheduler alert targets: %w", err)
	}
	// Notification targets are needed even without an alerts block when a
	// service enables monitoring; invalid ones are dropped for monitoring.
	if d.config.Notifications != nil {
		request.Targets = takod.AlertTargets{
			Slack:   deliverableTarget("slack", d.config.Notifications.Slack),
			Discord: deliverableTarget("discord", d.config.Notifications.Discord),
			Webhook: deliverableTarget("webhook", d.config.Notifications.Webhook),
		}
	}
	plan, err := d.buildHealthAlertPlan(request.Targets, d.healthCheckerNode(schedulable))
	if err != nil {
		return request, healthAlertPlan{}, err
	}
	if !alertRequestEnabled(request) && !plan.enabled() {
		request.Targets = takod.AlertTargets{}
	}
	return plan.apply(request), plan, nil
}

func (d *Deployer) warnf(format string, args ...any) {
	d.emitEvent(events.Event{
		Type: events.TypeWarning, Phase: events.PhaseDeploy, Level: events.LevelWarn,
		Message: fmt.Sprintf("  ⚠ %s\n", fmt.Sprintf(format, args...)),
	})
}

// healthCapable reports whether serverName advertises monitoring.health-v1.
func (d *Deployer) healthCapable(serverName string) (bool, error) {
	client, err := d.getRuntimeClient(serverName)
	if err != nil {
		return false, err
	}
	if err := d.ensureTakodCapability(client, serverName, takod.CapabilityMonitoringHealthV1, "node-side health monitoring"); err != nil {
		var capabilityErr *takodclient.CapabilityRequiredError
		if errors.As(err, &capabilityErr) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
