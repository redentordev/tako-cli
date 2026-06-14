package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	"github.com/redentordev/tako-cli/pkg/config"
)

const ConfigHashLabel = "tako.configHash"

type safeServiceConfigFingerprint struct {
	Build        string                    `json:"build,omitempty"`
	Image        string                    `json:"image,omitempty"`
	Port         int                       `json:"port,omitempty"`
	Command      string                    `json:"command,omitempty"`
	Replicas     int                       `json:"replicas,omitempty"`
	Restart      string                    `json:"restart,omitempty"`
	Volumes      []string                  `json:"volumes,omitempty"`
	Persistent   bool                      `json:"persistent,omitempty"`
	Proxy        *config.ProxyConfig       `json:"proxy,omitempty"`
	LoadBalancer config.LoadBalancerConfig `json:"loadBalancer,omitempty"`
	HealthCheck  config.HealthCheckConfig  `json:"healthCheck,omitempty"`
	Deploy       config.DeployConfig       `json:"deploy,omitempty"`
	Backup       *config.BackupConfig      `json:"backup,omitempty"`
	Monitoring   *config.MonitoringConfig  `json:"monitoring,omitempty"`
	Export       bool                      `json:"export,omitempty"`
	Imports      []string                  `json:"imports,omitempty"`
	Placement    *config.PlacementConfig   `json:"placement,omitempty"`
	DependsOn    []string                  `json:"dependsOn,omitempty"`
}

func SafeServiceConfigHash(service config.ServiceConfig) (string, bool) {
	if len(service.Env) > 0 || service.EnvFile != "" || len(service.Secrets) > 0 {
		return "", false
	}
	if service.Monitoring != nil && service.Monitoring.Webhook != "" {
		return "", false
	}

	fingerprint := safeServiceConfigFingerprint{
		Build:        service.Build,
		Image:        service.Image,
		Port:         service.Port,
		Command:      service.Command,
		Replicas:     service.Replicas,
		Restart:      service.Restart,
		Volumes:      sortedStrings(service.Volumes),
		Persistent:   service.Persistent,
		Proxy:        service.Proxy,
		LoadBalancer: service.LoadBalancer,
		HealthCheck:  service.HealthCheck,
		Deploy:       service.Deploy,
		Backup:       service.Backup,
		Monitoring:   service.Monitoring,
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

func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
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
