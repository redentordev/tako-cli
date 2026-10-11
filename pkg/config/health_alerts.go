package config

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Defaults and bounds for takod-run health monitoring
// (notifications.alerts.{services,host,containers,certificates} and
// services.<name>.monitoring).
const (
	DefaultMonitorInterval        = 60 * time.Second
	MinMonitorInterval            = 10 * time.Second
	MaxMonitorInterval            = time.Hour
	DefaultMonitorTimeout         = 10 * time.Second
	MinMonitorTimeout             = time.Second
	MaxMonitorTimeout             = 60 * time.Second
	DefaultMonitorAlertAfter      = 3
	DefaultMonitorRecoverAfter    = 2
	MaxMonitorDamping             = 100
	DefaultHostDiskPercent        = 90
	DefaultHostInodesPercent      = 90
	DefaultHostCPUFor             = 10 * time.Minute
	DefaultContainerCrashLoop     = 3
	DefaultContainerCrashWindow   = 10 * time.Minute
	DefaultCertificateWarnBefore  = 336 * time.Hour
	DefaultCertificateCritBefore  = 72 * time.Hour
	MaxMonitoredServicesPerEnv    = 200
	maxHealthAlertDuration        = 365 * 24 * time.Hour
	minHealthAlertWindow          = time.Minute
	maxHostLoadPerCore            = 1000.0
	maxContainerCrashLoopRestarts = 1000
)

// ExplicitHealthSections reports whether the alerts block explicitly sets a
// node-side health monitoring section (services, host, containers or
// certificates). Explicit sections require monitoring.health-v1 on every
// schedulable target server; default-derived sections never fail a deploy.
func (a *SchedulerAlertsConfig) ExplicitHealthSections() bool {
	return a != nil && (a.Services != nil || a.Host != nil || a.Containers != nil || a.Certificates != nil)
}

// MonitorTiming returns the effective check interval and timeout for a
// monitored service. Out-of-range intervals accepted by earlier releases are
// clamped to 10s..1h (and reported by ValidationWarnings); the timeout is
// always shorter than the interval.
func MonitorTiming(monitoring *MonitoringConfig) (time.Duration, time.Duration) {
	interval := DefaultMonitorInterval
	timeout := DefaultMonitorTimeout
	if monitoring == nil {
		return interval, timeout
	}
	if parsed, err := time.ParseDuration(strings.TrimSpace(monitoring.Interval)); err == nil && parsed > 0 {
		interval = parsed
	}
	if interval < MinMonitorInterval {
		interval = MinMonitorInterval
	}
	if interval > MaxMonitorInterval {
		interval = MaxMonitorInterval
	}
	if parsed, err := time.ParseDuration(strings.TrimSpace(monitoring.Timeout)); err == nil && parsed > 0 {
		timeout = parsed
	}
	if timeout >= interval {
		timeout = interval / 2
	}
	return interval, timeout
}

// MonitorDamping returns the effective alertAfter and recoverAfter for a
// monitored service: the service's own values, else
// notifications.alerts.services, else 3 and 2.
func MonitorDamping(alerts *SchedulerAlertsConfig, monitoring *MonitoringConfig) (int, int) {
	alertAfter, recoverAfter := DefaultMonitorAlertAfter, DefaultMonitorRecoverAfter
	if alerts != nil && alerts.Services != nil {
		if alerts.Services.AlertAfter > 0 {
			alertAfter = alerts.Services.AlertAfter
		}
		if alerts.Services.RecoverAfter > 0 {
			recoverAfter = alerts.Services.RecoverAfter
		}
	}
	if monitoring != nil {
		if monitoring.AlertAfter > 0 {
			alertAfter = monitoring.AlertAfter
		}
		if monitoring.RecoverAfter > 0 {
			recoverAfter = monitoring.RecoverAfter
		}
	}
	return alertAfter, recoverAfter
}

// HostDiskThreshold returns the effective disk percent (0 disables).
func (h *HostAlertsConfig) HostDiskThreshold() int {
	if h == nil || h.Disk == nil {
		return DefaultHostDiskPercent
	}
	return *h.Disk
}

// HostInodesThreshold returns the effective inode percent (0 disables).
func (h *HostAlertsConfig) HostInodesThreshold() int {
	if h == nil || h.Inodes == nil {
		return DefaultHostInodesPercent
	}
	return *h.Inodes
}

// HostConditionsEnabled reports whether node-condition alerts are on.
func (h *HostAlertsConfig) HostConditionsEnabled() bool {
	return h == nil || h.Conditions == nil || *h.Conditions
}

// ContainerOOMEnabled reports whether OOM-kill events are on.
func (c *ContainerAlertsConfig) ContainerOOMEnabled() bool {
	return c == nil || c.OOM == nil || *c.OOM
}

// ContainerCrashLoopThreshold returns the restart threshold (0 disables).
func (c *ContainerAlertsConfig) ContainerCrashLoopThreshold() int {
	if c == nil || c.CrashLoop == nil {
		return DefaultContainerCrashLoop
	}
	return *c.CrashLoop
}

func durationOrDefault(value string, fallback time.Duration) time.Duration {
	if parsed, err := time.ParseDuration(strings.TrimSpace(value)); err == nil && parsed > 0 {
		return parsed
	}
	return fallback
}

// HostCPUForDuration returns the effective sustained-CPU window.
func (h *HostAlertsConfig) HostCPUForDuration() time.Duration {
	if h == nil {
		return DefaultHostCPUFor
	}
	return durationOrDefault(h.CPUFor, DefaultHostCPUFor)
}

// CrashLoopWindowDuration returns the effective crash-loop window.
func (c *ContainerAlertsConfig) CrashLoopWindowDuration() time.Duration {
	if c == nil {
		return DefaultContainerCrashWindow
	}
	return durationOrDefault(c.CrashLoopWindow, DefaultContainerCrashWindow)
}

// CertificateThresholds returns the effective warning and critical windows.
func (c *CertificateAlertsConfig) CertificateThresholds() (time.Duration, time.Duration) {
	if c == nil {
		return DefaultCertificateWarnBefore, DefaultCertificateCritBefore
	}
	return durationOrDefault(c.WarnBefore, DefaultCertificateWarnBefore), durationOrDefault(c.CriticalBefore, DefaultCertificateCritBefore)
}

func validateHealthAlertSections(alerts *SchedulerAlertsConfig) error {
	if alerts == nil {
		return nil
	}
	if services := alerts.Services; services != nil {
		if err := validateDampingValue("notifications.alerts.services.alertAfter", services.AlertAfter); err != nil {
			return err
		}
		if err := validateDampingValue("notifications.alerts.services.recoverAfter", services.RecoverAfter); err != nil {
			return err
		}
	}
	if host := alerts.Host; host != nil {
		for _, percent := range []struct {
			name  string
			value *int
		}{{"disk", host.Disk}, {"inodes", host.Inodes}, {"memory", &host.Memory}, {"cpu", &host.CPU}} {
			if percent.value != nil && (*percent.value < 0 || *percent.value > 99) {
				return fmt.Errorf("notifications.alerts.host.%s must be a percent between 1 and 99, or 0 to disable", percent.name)
			}
		}
		if host.Load < 0 || host.Load > maxHostLoadPerCore {
			return fmt.Errorf("notifications.alerts.host.load must be between 0 and %g", maxHostLoadPerCore)
		}
		if err := validateHealthDuration("notifications.alerts.host.cpuFor", host.CPUFor, minHealthAlertWindow, 24*time.Hour); err != nil {
			return err
		}
	}
	if containers := alerts.Containers; containers != nil {
		if containers.CrashLoop != nil && (*containers.CrashLoop < 0 || *containers.CrashLoop > maxContainerCrashLoopRestarts) {
			return fmt.Errorf("notifications.alerts.containers.crashLoop must be between 0 and %d", maxContainerCrashLoopRestarts)
		}
		if err := validateHealthDuration("notifications.alerts.containers.crashLoopWindow", containers.CrashLoopWindow, minHealthAlertWindow, 24*time.Hour); err != nil {
			return err
		}
	}
	if certificates := alerts.Certificates; certificates != nil {
		if err := validateHealthDuration("notifications.alerts.certificates.warnBefore", certificates.WarnBefore, time.Hour, maxHealthAlertDuration); err != nil {
			return err
		}
		if err := validateHealthDuration("notifications.alerts.certificates.criticalBefore", certificates.CriticalBefore, time.Hour, maxHealthAlertDuration); err != nil {
			return err
		}
		warn, critical := certificates.CertificateThresholds()
		if critical > warn {
			return fmt.Errorf("notifications.alerts.certificates.criticalBefore must not exceed warnBefore")
		}
	}
	return nil
}

func validateDampingValue(path string, value int) error {
	if value < 0 || value > MaxMonitorDamping {
		return fmt.Errorf("%s must be between 1 and %d", path, MaxMonitorDamping)
	}
	return nil
}

func validateHealthDuration(path, value string, minimum, maximum time.Duration) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || parsed < minimum || parsed > maximum {
		return fmt.Errorf("%s must be a duration between %s and %s", path, minimum, maximum)
	}
	if parsed%time.Second != 0 {
		return fmt.Errorf("%s must use whole seconds", path)
	}
	return nil
}

// validateServiceMonitoring validates a service's monitoring block. Keys
// introduced with node-side monitoring are strict; values accepted by
// earlier releases (an out-of-range interval, an invalid webhook) are left
// to ValidationWarnings so existing configs keep loading.
func validateServiceMonitoring(name string, service *ServiceConfig, cfg *Config) error {
	monitoring := service.Monitoring
	if monitoring == nil || !monitoring.Enabled {
		return nil
	}
	if monitoring.Interval == "" {
		monitoring.Interval = "60s"
	}
	if strings.TrimSpace(monitoring.Webhook) == "" && !notificationTargetsConfigured(cfg) {
		return fmt.Errorf("service %s: monitoring enabled but no webhook configured (set monitoring.webhook or a notifications slack, discord or webhook target)", name)
	}
	if monitoring.CheckType == "" {
		if service.IsPublic() && service.HealthCheck.Path != "" {
			monitoring.CheckType = "http"
		} else {
			monitoring.CheckType = "container"
		}
	}
	if monitoring.CheckType != "http" && monitoring.CheckType != "container" {
		return fmt.Errorf("service %s: invalid monitoring checkType: %s (must be: http or container)", name, monitoring.CheckType)
	}
	if strings.TrimSpace(monitoring.Timeout) != "" {
		timeout, err := time.ParseDuration(strings.TrimSpace(monitoring.Timeout))
		if err != nil || timeout < MinMonitorTimeout || timeout > MaxMonitorTimeout {
			return fmt.Errorf("service %s: monitoring.timeout must be a duration between %s and %s", name, MinMonitorTimeout, MaxMonitorTimeout)
		}
		interval, _ := MonitorTiming(monitoring)
		if timeout >= interval {
			return fmt.Errorf("service %s: monitoring.timeout must be shorter than monitoring.interval", name)
		}
	}
	if err := validateDampingValue("service "+name+": monitoring.alertAfter", monitoring.AlertAfter); err != nil {
		return err
	}
	return validateDampingValue("service "+name+": monitoring.recoverAfter", monitoring.RecoverAfter)
}

func notificationTargetsConfigured(cfg *Config) bool {
	if cfg == nil || cfg.Notifications == nil {
		return false
	}
	n := cfg.Notifications
	return strings.TrimSpace(n.Slack) != "" || strings.TrimSpace(n.Discord) != "" || strings.TrimSpace(n.Webhook) != ""
}

func validateMonitoredServiceCount(envName string, env *EnvironmentConfig) error {
	count := 0
	for _, service := range env.Services {
		if service.Monitoring != nil && service.Monitoring.Enabled {
			count++
		}
	}
	if count > MaxMonitoredServicesPerEnv {
		return fmt.Errorf("environment %s: at most %d services may enable monitoring (found %d)", envName, MaxMonitoredServicesPerEnv, count)
	}
	return nil
}

// monitoringWarnings reports monitoring values that earlier releases
// accepted but node-side monitoring cannot use as written: a webhook failing
// the alert target rules (ignored, never sent or used) and an interval
// outside 10s..1h (clamped).
func monitoringWarnings(cfg *Config) []ValidationWarning {
	var warnings []ValidationWarning
	environmentNames := make([]string, 0, len(cfg.Environments))
	for name := range cfg.Environments {
		environmentNames = append(environmentNames, name)
	}
	sort.Strings(environmentNames)
	for _, environmentName := range environmentNames {
		environment := cfg.Environments[environmentName]
		serviceNames := make([]string, 0, len(environment.Services))
		for name := range environment.Services {
			serviceNames = append(serviceNames, name)
		}
		sort.Strings(serviceNames)
		for _, serviceName := range serviceNames {
			monitoring := environment.Services[serviceName].Monitoring
			if monitoring == nil {
				continue
			}
			if monitoring.WebhookDeprecated() {
				path := fmt.Sprintf("environments.%s.services.%s.monitoring.webhook", environmentName, serviceName)
				warnings = append(warnings, ValidationWarning{
					Environment: environmentName,
					Service:     serviceName,
					Field:       "monitoring.webhook",
					Message:     fmt.Sprintf("%s is not a public HTTPS alert target; it is ignored and never sent to takod or used for delivery. It will be rejected starting in %s.", path, DeprecatedKeysRemovalRelease),
				})
			}
			if monitoring.Enabled && strings.TrimSpace(monitoring.Interval) != "" {
				parsed, err := time.ParseDuration(strings.TrimSpace(monitoring.Interval))
				if err != nil || parsed < MinMonitorInterval || parsed > MaxMonitorInterval {
					effective, _ := MonitorTiming(monitoring)
					warnings = append(warnings, ValidationWarning{
						Environment: environmentName,
						Service:     serviceName,
						Field:       "monitoring.interval",
						Message:     fmt.Sprintf("environments.%s.services.%s.monitoring.interval %q is outside %s..%s; %s is used", environmentName, serviceName, monitoring.Interval, MinMonitorInterval, MaxMonitorInterval, effective),
					})
				}
			}
		}
	}
	return warnings
}
