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
	Build        string                      `json:"build,omitempty"`
	Dockerfile   string                      `json:"dockerfile,omitempty"`
	Image        string                      `json:"image,omitempty"`
	Platform     string                      `json:"platform,omitempty"`
	Port         int                         `json:"port,omitempty"`
	Ports        []config.PortConfig         `json:"ports,omitempty"`
	Command      string                      `json:"command,omitempty"`
	Replicas     int                         `json:"replicas,omitempty"`
	Restart      string                      `json:"restart,omitempty"`
	Volumes      []string                    `json:"volumes,omitempty"`
	Configs      []safeConfigFileMount       `json:"configs,omitempty"`
	Persistent   bool                        `json:"persistent,omitempty"`
	Proxy        *config.ProxyConfig         `json:"proxy,omitempty"`
	LoadBalancer config.LoadBalancerConfig   `json:"loadBalancer,omitempty"`
	HealthCheck  config.HealthCheckConfig    `json:"healthCheck,omitempty"`
	Deploy       config.DeployConfig         `json:"deploy,omitempty"`
	Hooks        config.HooksConfig          `json:"hooks,omitempty"`
	Backup       *config.BackupConfig        `json:"backup,omitempty"`
	Monitoring   *config.MonitoringConfig    `json:"monitoring,omitempty"`
	Export       *config.ServiceExportConfig `json:"export,omitempty"`
	Placement    *config.PlacementConfig     `json:"placement,omitempty"`
	DependsOn    []string                    `json:"dependsOn,omitempty"`
}

type safeConfigFileMount struct {
	Source      string `json:"source"`
	Target      string `json:"target"`
	Mode        string `json:"mode,omitempty"`
	ContentHash string `json:"contentHash,omitempty"`
}

func SafeServiceConfigHash(service config.ServiceConfig) (string, bool) {
	if len(service.Env) > 0 || service.EnvFile != "" || len(service.Secrets) > 0 {
		return "", false
	}
	if len(service.Volumes) > 0 {
		return "", false
	}
	if hooksContainEnvOrSecrets(service.Hooks) {
		return "", false
	}
	if service.Monitoring != nil && service.Monitoring.Webhook != "" {
		return "", false
	}

	fingerprint := safeServiceConfigFingerprint{
		Build:        service.Build,
		Dockerfile:   service.Dockerfile,
		Image:        service.Image,
		Platform:     service.Platform,
		Port:         service.Port,
		Ports:        sortedPortConfigs(service.Ports),
		Command:      service.Command,
		Replicas:     service.Replicas,
		Restart:      service.Restart,
		Configs:      sortedConfigFileMounts(service.Configs),
		Persistent:   service.Persistent,
		Proxy:        service.Proxy,
		LoadBalancer: service.LoadBalancer,
		HealthCheck:  service.HealthCheck,
		Deploy:       service.Deploy,
		Hooks:        service.Hooks,
		Backup:       service.Backup,
		Monitoring:   service.Monitoring,
		Export:       cloneServiceExport(service.Export),
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

func hooksContainEnvOrSecrets(hooks config.HooksConfig) bool {
	for _, hook := range []*config.HookConfig{hooks.PreDeploy, hooks.PostDeploy} {
		if hook == nil {
			continue
		}
		if len(hook.Env) > 0 || len(hook.Secrets) > 0 {
			return true
		}
	}
	return false
}

func sortedStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

func sortedPortConfigs(ports []config.PortConfig) []config.PortConfig {
	if len(ports) == 0 {
		return nil
	}
	service := config.ServiceConfig{Ports: ports}
	out := service.EffectivePorts()
	for index := range out {
		if out[index].Proxy != nil && len(out[index].Proxy.RedirectFrom) > 0 {
			proxy := *out[index].Proxy
			proxy.RedirectFrom = sortedStrings(proxy.RedirectFrom)
			out[index].Proxy = &proxy
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].Target < out[j].Target
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func sortedConfigFileMounts(configs []config.ServiceConfigFileMount) []safeConfigFileMount {
	if len(configs) == 0 {
		return nil
	}
	out := make([]safeConfigFileMount, 0, len(configs))
	for _, cfg := range configs {
		out = append(out, safeConfigFileMount{
			Source:      cfg.Source,
			Target:      cfg.Target,
			Mode:        cfg.Mode,
			ContentHash: cfg.ContentHash,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Source == out[j].Source {
			return out[i].Target < out[j].Target
		}
		return out[i].Source < out[j].Source
	})
	return out
}

func cloneServiceExport(export *config.ServiceExportConfig) *config.ServiceExportConfig {
	if export == nil {
		return nil
	}
	clone := &config.ServiceExportConfig{}
	if len(export.Ports) > 0 {
		clone.Ports = make(map[string]int, len(export.Ports))
		for name, target := range export.Ports {
			clone.Ports[name] = target
		}
	}
	return clone
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
