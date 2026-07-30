package deployer

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func (d *Deployer) PreflightAlertPolicy() error {
	request, err := d.buildAlertConfigRequest()
	if err != nil {
		return err
	}
	if !alertRequestEnabled(request) {
		return nil
	}
	targetServers, err := d.alertTargetServers(true)
	if err != nil {
		return fmt.Errorf("resolve scheduler alert targets: %w", err)
	}
	if len(targetServers) == 0 {
		return nil
	}
	return d.preflightTakodCapability(targetServers, takod.CapabilitySchedulerAlertsV1, "durable scheduler alerts")
}

func (d *Deployer) ApplyAlertPolicy() error {
	request, err := d.buildAlertConfigRequest()
	if err != nil {
		return err
	}
	allServers, err := d.alertTargetServers(false)
	if err != nil {
		return fmt.Errorf("resolve scheduler alert targets: %w", err)
	}
	if !alertRequestEnabled(request) {
		return d.applyAlertRequest(allServers, request)
	}
	targetServers, err := d.alertTargetServers(true)
	if err != nil {
		return fmt.Errorf("resolve scheduler alert targets: %w", err)
	}
	targetSet := make(map[string]bool, len(targetServers))
	for _, serverName := range targetServers {
		targetSet[serverName] = true
	}
	enableErr := d.applyAlertRequest(targetServers, request)
	if enableErr != nil {
		return enableErr
	}
	disabled := request
	disabled.Policy = takod.AlertPolicy{}
	disabled.Targets = takod.AlertTargets{}
	var staleServers []string
	for _, serverName := range allServers {
		if !targetSet[serverName] {
			staleServers = append(staleServers, serverName)
		}
	}
	return d.applyAlertRequest(staleServers, disabled)
}

func (d *Deployer) applyAlertRequest(targetServers []string, request takod.AlertConfigRequest) error {
	return runTakodNodeActions(targetServers, func(serverName string) error {
		client, err := d.getRuntimeClient(serverName)
		if err != nil {
			return err
		}
		if !alertRequestEnabled(request) {
			if err := d.ensureTakodCapability(client, serverName, takod.CapabilitySchedulerAlertsV1, "durable scheduler alerts"); err != nil {
				var capabilityErr *takodclient.CapabilityRequiredError
				if errors.As(err, &capabilityErr) {
					return nil
				}
				return err
			}
		}
		if _, err := takodclient.RequestJSONWithContext(d.baseContext(), client, d.takodSocket(), "PUT", takodclient.AlertsEndpoint("", ""), request); err != nil {
			return fmt.Errorf("apply scheduler alert policy on %s: %w", serverName, err)
		}
		return nil
	})
}

func (d *Deployer) alertTargetServers(enabled bool) ([]string, error) {
	servers, err := d.config.GetEnvironmentServers(d.environment)
	if err != nil {
		return nil, err
	}
	if !enabled {
		sort.Strings(servers)
		return servers, nil
	}
	return config.ResolveSchedulableEnvironmentTargets(d.config.Servers, servers, d.environment)
}

func alertRequestEnabled(request takod.AlertConfigRequest) bool {
	policy := request.Policy
	return policy.JobConsecutiveFailures > 0 || policy.JobConsecutiveTimeouts > 0 || policy.JobConsecutiveSkipped > 0 || policy.JobNoSuccessSeconds > 0 || policy.BackupMaxAgeSeconds > 0
}

func (d *Deployer) buildAlertConfigRequest() (takod.AlertConfigRequest, error) {
	request := takod.AlertConfigRequest{Project: d.config.Project.Name, Environment: d.environment}
	if d.config.Notifications == nil || d.config.Notifications.Alerts == nil {
		return request, nil
	}
	alerts := d.config.Notifications.Alerts
	request.Policy.JobConsecutiveFailures = alerts.Jobs.ConsecutiveFailures
	request.Policy.JobConsecutiveTimeouts = alerts.Jobs.ConsecutiveTimeouts
	request.Policy.JobConsecutiveSkipped = alerts.Jobs.ConsecutiveSkipped
	var err error
	request.Policy.JobNoSuccessSeconds, err = alertDurationSeconds(alerts.Jobs.NoSuccessFor)
	if err != nil {
		return takod.AlertConfigRequest{}, fmt.Errorf("notifications.alerts.jobs.noSuccessFor: %w", err)
	}
	request.Policy.BackupMaxAgeSeconds, err = alertDurationSeconds(alerts.Backups.MaxAge)
	if err != nil {
		return takod.AlertConfigRequest{}, fmt.Errorf("notifications.alerts.backups.maxAge: %w", err)
	}
	request.Targets = takod.AlertTargets{
		Slack: strings.TrimSpace(d.config.Notifications.Slack), Discord: strings.TrimSpace(d.config.Notifications.Discord), Webhook: strings.TrimSpace(d.config.Notifications.Webhook),
	}
	return request, nil
}

func alertDurationSeconds(value string) (int64, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	return int64(duration / time.Second), nil
}
