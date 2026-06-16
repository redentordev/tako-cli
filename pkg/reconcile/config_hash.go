package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

const ConfigHashLabel = "tako.configHash"

type safeServiceConfigFingerprint struct {
	Build        string                    `json:"build,omitempty"`
	Dockerfile   string                    `json:"dockerfile,omitempty"`
	Image        string                    `json:"image,omitempty"`
	Port         int                       `json:"port,omitempty"`
	Command      string                    `json:"command,omitempty"`
	Replicas     int                       `json:"replicas,omitempty"`
	Restart      string                    `json:"restart,omitempty"`
	EnvKeys      []string                  `json:"envKeys,omitempty"`
	EnvFile      string                    `json:"envFile,omitempty"`
	Secrets      []string                  `json:"secrets,omitempty"`
	Volumes      []string                  `json:"volumes,omitempty"`
	Persistent   bool                      `json:"persistent,omitempty"`
	Proxy        *config.ProxyConfig       `json:"proxy,omitempty"`
	LoadBalancer config.LoadBalancerConfig `json:"loadBalancer,omitempty"`
	HealthCheck  config.HealthCheckConfig  `json:"healthCheck,omitempty"`
	Deploy       config.DeployConfig       `json:"deploy,omitempty"`
	Backup       *config.BackupConfig      `json:"backup,omitempty"`
	Monitoring   *monitoringFingerprint    `json:"monitoring,omitempty"`
	Export       bool                      `json:"export,omitempty"`
	Imports      []string                  `json:"imports,omitempty"`
	Placement    *config.PlacementConfig   `json:"placement,omitempty"`
	DependsOn    []string                  `json:"dependsOn,omitempty"`
}

type monitoringFingerprint struct {
	Enabled           bool   `json:"enabled"`
	Interval          string `json:"interval,omitempty"`
	WebhookConfigured bool   `json:"webhookConfigured,omitempty"`
	CheckType         string `json:"checkType,omitempty"`
}

func SafeServiceConfigHash(service config.ServiceConfig) (string, bool) {
	fingerprint := safeServiceConfigFingerprint{
		Build:        service.Build,
		Dockerfile:   service.Dockerfile,
		Image:        service.Image,
		Port:         service.Port,
		Command:      service.Command,
		Replicas:     service.Replicas,
		Restart:      service.Restart,
		EnvKeys:      sortedMapKeys(service.Env),
		EnvFile:      service.EnvFile,
		Secrets:      sortedStrings(service.Secrets),
		Volumes:      sortedStrings(service.Volumes),
		Persistent:   service.Persistent,
		Proxy:        service.Proxy,
		LoadBalancer: service.LoadBalancer,
		HealthCheck:  service.HealthCheck,
		Deploy:       service.Deploy,
		Backup:       service.Backup,
		Monitoring:   cloneMonitoringFingerprint(service.Monitoring),
		Export:       service.Export,
		Imports:      sortedStrings(service.Imports),
		Placement:    clonePlacement(service.Placement),
		DependsOn:    sortedStrings(service.DependsOn),
	}
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}

func sortedMapKeys(values map[string]string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func cloneMonitoringFingerprint(monitoring *config.MonitoringConfig) *monitoringFingerprint {
	if monitoring == nil {
		return nil
	}
	return &monitoringFingerprint{
		Enabled:           monitoring.Enabled,
		Interval:          monitoring.Interval,
		WebhookConfigured: strings.TrimSpace(monitoring.Webhook) != "",
		CheckType:         monitoring.CheckType,
	}
}

func clonePlacement(placement *config.PlacementConfig) *config.PlacementConfig {
	if placement == nil {
		return nil
	}
	clone := *placement
	clone.Servers = sortedStrings(placement.Servers)
	clone.Constraints = sortedStrings(placement.Constraints)
	clone.Preferences = sortedStrings(placement.Preferences)
	return &clone
}
