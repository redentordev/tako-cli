package config

import (
	"strings"
	"testing"
	"time"
)

func intPointer(value int) *int { return &value }

func TestValidateHealthAlertSections(t *testing.T) {
	base := func() *NotificationsConfig {
		return &NotificationsConfig{Webhook: "https://alerts.example.invalid/hook", Alerts: &SchedulerAlertsConfig{}}
	}
	valid := base()
	valid.Alerts.Services = &ServiceAlertsConfig{AlertAfter: 3, RecoverAfter: 2}
	valid.Alerts.Host = &HostAlertsConfig{Disk: intPointer(85), Memory: 90, CPU: 90, CPUFor: "10m", Load: 2}
	valid.Alerts.Containers = &ContainerAlertsConfig{CrashLoop: intPointer(5), CrashLoopWindow: "15m"}
	valid.Alerts.Certificates = &CertificateAlertsConfig{WarnBefore: "336h", CriticalBefore: "72h"}
	if err := ValidateNotificationsConfig(valid); err != nil {
		t.Fatalf("valid sections rejected: %v", err)
	}
	invalid := map[string]func(*SchedulerAlertsConfig){
		"disk over 99":         func(a *SchedulerAlertsConfig) { a.Host = &HostAlertsConfig{Disk: intPointer(100)} },
		"negative memory":      func(a *SchedulerAlertsConfig) { a.Host = &HostAlertsConfig{Memory: -1} },
		"cpuFor too short":     func(a *SchedulerAlertsConfig) { a.Host = &HostAlertsConfig{CPUFor: "1s"} },
		"damping above max":    func(a *SchedulerAlertsConfig) { a.Services = &ServiceAlertsConfig{AlertAfter: 101} },
		"crash window garbage": func(a *SchedulerAlertsConfig) { a.Containers = &ContainerAlertsConfig{CrashLoopWindow: "soon"} },
		"critical > warn": func(a *SchedulerAlertsConfig) {
			a.Certificates = &CertificateAlertsConfig{WarnBefore: "24h", CriticalBefore: "48h"}
		},
	}
	for name, mutate := range invalid {
		notifications := base()
		mutate(notifications.Alerts)
		if err := ValidateNotificationsConfig(notifications); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Host alerts need a project-wide target; service damping alone does not.
	hostOnly := &NotificationsConfig{Alerts: &SchedulerAlertsConfig{Host: &HostAlertsConfig{}}}
	if err := ValidateNotificationsConfig(hostOnly); err == nil {
		t.Fatalf("host alerts without target accepted")
	}
	servicesOnly := &NotificationsConfig{Alerts: &SchedulerAlertsConfig{Services: &ServiceAlertsConfig{AlertAfter: 2}}}
	if err := ValidateNotificationsConfig(servicesOnly); err != nil {
		t.Fatalf("services-only damping rejected: %v", err)
	}
}

func TestHealthAlertDefaults(t *testing.T) {
	var host *HostAlertsConfig
	if host.HostDiskThreshold() != 90 || host.HostInodesThreshold() != 90 || !host.HostConditionsEnabled() || host.HostCPUForDuration() != 10*time.Minute {
		t.Fatalf("host defaults wrong")
	}
	if (&HostAlertsConfig{Disk: intPointer(0)}).HostDiskThreshold() != 0 {
		t.Fatalf("disk: 0 must disable")
	}
	var containers *ContainerAlertsConfig
	if !containers.ContainerOOMEnabled() || containers.ContainerCrashLoopThreshold() != 3 || containers.CrashLoopWindowDuration() != 10*time.Minute {
		t.Fatalf("container defaults wrong")
	}
	warn, critical := (*CertificateAlertsConfig)(nil).CertificateThresholds()
	if warn != 14*24*time.Hour || critical != 3*24*time.Hour {
		t.Fatalf("certificate defaults = %v %v", warn, critical)
	}
	alertAfter, recoverAfter := MonitorDamping(&SchedulerAlertsConfig{Services: &ServiceAlertsConfig{AlertAfter: 5}}, &MonitoringConfig{RecoverAfter: 4})
	if alertAfter != 5 || recoverAfter != 4 {
		t.Fatalf("damping = %d %d", alertAfter, recoverAfter)
	}
	interval, timeout := MonitorTiming(&MonitoringConfig{Interval: "5s"})
	if interval != 10*time.Second || timeout != 5*time.Second {
		t.Fatalf("clamped timing = %v %v", interval, timeout)
	}
	interval, timeout = MonitorTiming(nil)
	if interval != time.Minute || timeout != 10*time.Second {
		t.Fatalf("default timing = %v %v", interval, timeout)
	}
}

func TestValidateServiceMonitoringStrictNewKeys(t *testing.T) {
	cfg := &Config{Notifications: &NotificationsConfig{Slack: "https://hooks.slack.com/services/example"}}
	cases := map[string]MonitoringConfig{
		"timeout too long":      {Enabled: true, Timeout: "2m"},
		"timeout over interval": {Enabled: true, Interval: "10s", Timeout: "10s"},
		"alertAfter too big":    {Enabled: true, AlertAfter: 500},
		"bad check type":        {Enabled: true, CheckType: "tcp"},
	}
	for name, monitoring := range cases {
		monitoring := monitoring
		service := &ServiceConfig{Monitoring: &monitoring}
		if err := validateServiceMonitoring("web", service, cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Previously accepted values keep loading.
	legacy := MonitoringConfig{Enabled: true, Interval: "1s", Webhook: "http://10.0.0.5/hook"}
	if err := validateServiceMonitoring("web", &ServiceConfig{Monitoring: &legacy}, &Config{}); err != nil {
		t.Fatalf("previously valid monitoring rejected: %v", err)
	}
	// Without any target, monitoring still needs a webhook or notification target.
	none := MonitoringConfig{Enabled: true}
	if err := validateServiceMonitoring("web", &ServiceConfig{Monitoring: &none}, &Config{}); err == nil {
		t.Fatalf("monitoring without any target accepted")
	}
}

func TestMonitoringWarningsForDeprecatedWebhookAndInterval(t *testing.T) {
	cfg := &Config{Environments: map[string]EnvironmentConfig{
		"production": {Services: map[string]ServiceConfig{
			"web": {Monitoring: &MonitoringConfig{Enabled: true, Interval: "2h", Webhook: "http://10.0.0.5/hook"}},
			"api": {Monitoring: &MonitoringConfig{Enabled: true, Webhook: "https://alerts.example.invalid/api"}},
		}},
	}}
	warnings := monitoringWarnings(cfg)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %+v", warnings)
	}
	fields := warnings[0].Field + "," + warnings[1].Field
	if fields != "monitoring.webhook,monitoring.interval" {
		t.Fatalf("fields = %s", fields)
	}
	if !strings.Contains(warnings[0].Message, "environments.production.services.web.monitoring.webhook") || !strings.Contains(warnings[0].Message, "never sent") {
		t.Fatalf("webhook warning = %q", warnings[0].Message)
	}
	web := cfg.Environments["production"].Services["web"].Monitoring
	if web.DeliverableWebhook() != "" || !web.WebhookDeprecated() {
		t.Fatalf("private webhook must not be deliverable")
	}
	api := cfg.Environments["production"].Services["api"].Monitoring
	if api.DeliverableWebhook() == "" || api.WebhookDeprecated() {
		t.Fatalf("public webhook must be deliverable")
	}
}
