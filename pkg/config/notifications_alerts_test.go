package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateNotificationsConfigAlerts(t *testing.T) {
	tests := []struct {
		name    string
		config  *NotificationsConfig
		wantErr string
	}{
		{name: "valid", config: &NotificationsConfig{Webhook: "https://alerts.example/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 2, NoSuccessFor: "30m"}, Backups: BackupAlertsConfig{MaxAge: "25h"}}}},
		{name: "missing target", config: &NotificationsConfig{Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}}}, wantErr: "requires at least one"},
		{name: "short duration", config: &NotificationsConfig{Webhook: "https://alerts.example/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{NoSuccessFor: "59s"}}}, wantErr: "between 1m"},
		{name: "fractional seconds", config: &NotificationsConfig{Webhook: "https://alerts.example/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{NoSuccessFor: "1m500ms"}}}, wantErr: "whole seconds"},
		{name: "negative threshold", config: &NotificationsConfig{Webhook: "https://alerts.example/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveSkipped: -1}}}, wantErr: "between 0 and 1000"},
		{name: "insecure webhook", config: &NotificationsConfig{Webhook: "http://alerts.example/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}}}, wantErr: "absolute HTTPS"},
		{name: "unofficial slack", config: &NotificationsConfig{Slack: "https://alerts.example/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}}}, wantErr: "official Slack"},
		{name: "private literal", config: &NotificationsConfig{Webhook: "https://127.0.0.1/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}}}, wantErr: "special-use"},
		{name: "cgnat literal", config: &NotificationsConfig{Webhook: "https://100.64.0.1/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}}}, wantErr: "special-use"},
		{name: "documentation literal", config: &NotificationsConfig{Webhook: "https://192.0.2.1/token", Alerts: &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}}}, wantErr: "special-use"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateNotificationsConfig(test.config)
			if test.wantErr == "" && err != nil {
				t.Fatalf("ValidateNotificationsConfig() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("ValidateNotificationsConfig() error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestValidateNotificationsConfigNormalizesAlertTargets(t *testing.T) {
	notifications := &NotificationsConfig{
		Webhook: "  https://alerts.example/token  ",
		Alerts:  &SchedulerAlertsConfig{Jobs: JobAlertsConfig{ConsecutiveFailures: 1}},
	}
	if err := ValidateNotificationsConfig(notifications); err != nil {
		t.Fatal(err)
	}
	if notifications.Webhook != "https://alerts.example/token" {
		t.Fatalf("webhook = %q", notifications.Webhook)
	}
}

func TestNotificationsAlertsYAMLRoundTrip(t *testing.T) {
	original := NotificationsConfig{
		Webhook: "https://alerts.example/token",
		Alerts: &SchedulerAlertsConfig{
			Jobs:    JobAlertsConfig{ConsecutiveFailures: 2, ConsecutiveTimeouts: 1, ConsecutiveSkipped: 3, NoSuccessFor: "45m"},
			Backups: BackupAlertsConfig{MaxAge: "25h"},
		},
	}
	data, err := yaml.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip NotificationsConfig
	if err := yaml.Unmarshal(data, &roundTrip); err != nil {
		t.Fatal(err)
	}
	if roundTrip.Alerts == nil || roundTrip.Alerts.Jobs != original.Alerts.Jobs || roundTrip.Alerts.Backups != original.Alerts.Backups || roundTrip.Webhook != original.Webhook {
		t.Fatalf("round trip = %#v", roundTrip)
	}
}
