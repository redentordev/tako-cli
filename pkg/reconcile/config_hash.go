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

const RevisionLabel = "tako.revision"

const DeployStrategyLabel = "tako.deployStrategy"

const SlotLabel = "tako.slot"

const ActiveLabel = "tako.active"

type safeServiceConfigFingerprint struct {
	Kind             string                         `json:"kind,omitempty"`
	Schedule         string                         `json:"schedule,omitempty"`
	Timezone         string                         `json:"timezone,omitempty"`
	Timeout          string                         `json:"timeout,omitempty"`
	Build            string                         `json:"build,omitempty"`
	BuildArgs        map[string]string              `json:"buildArgs,omitempty"`
	BuildTarget      string                         `json:"buildTarget,omitempty"`
	Dockerfile       string                         `json:"dockerfile,omitempty"`
	Image            string                         `json:"image,omitempty"`
	ImageFrom        string                         `json:"imageFrom,omitempty"`
	Port             int                            `json:"port,omitempty"`
	Ports            []string                       `json:"ports,omitempty"`
	Command          any                            `json:"command,omitempty"`
	Entrypoint       any                            `json:"entrypoint,omitempty"`
	Labels           map[string]string              `json:"labels,omitempty"`
	Replicas         int                            `json:"replicas,omitempty"`
	Restart          string                         `json:"restart,omitempty"`
	EnvKeys          []string                       `json:"envKeys,omitempty"`
	EnvFile          string                         `json:"envFile,omitempty"`
	EnvFiles         []string                       `json:"envFiles,omitempty"`
	RunInputHash     string                         `json:"runInputHash,omitempty"`
	User             string                         `json:"user,omitempty"`
	WorkingDir       string                         `json:"workingDir,omitempty"`
	StopGracePeriod  string                         `json:"stopGracePeriod,omitempty"`
	Init             bool                           `json:"init,omitempty"`
	ExtraHosts       []string                       `json:"extraHosts,omitempty"`
	Ulimits          map[string]config.UlimitConfig `json:"ulimits,omitempty"`
	ShmSize          string                         `json:"shmSize,omitempty"`
	Secrets          []string                       `json:"secrets,omitempty"`
	Volumes          []string                       `json:"volumes,omitempty"`
	Files            []serviceFileFingerprint       `json:"files,omitempty"`
	FilesContentHash string                         `json:"filesContentHash,omitempty"`
	Persistent       bool                           `json:"persistent,omitempty"`
	Proxy            *config.ProxyConfig            `json:"proxy,omitempty"`
	LoadBalancer     config.LoadBalancerConfig      `json:"loadBalancer,omitempty"`
	HealthCheck      config.HealthCheckConfig       `json:"healthCheck,omitempty"`
	Deploy           config.DeployConfig            `json:"deploy,omitempty"`
	Backup           *backupFingerprint             `json:"backup,omitempty"`
	Monitoring       *monitoringFingerprint         `json:"monitoring,omitempty"`
	Export           bool                           `json:"export,omitempty"`
	Imports          []string                       `json:"imports,omitempty"`
	Placement        *config.PlacementConfig        `json:"placement,omitempty"`
	DependsOn        []string                       `json:"dependsOn,omitempty"`
	Resources        *config.ResourceLimitsConfig   `json:"resources,omitempty"`
}

type serviceFileFingerprint struct {
	Target string `json:"target"`
	Secret bool   `json:"secret,omitempty"`
	Owner  string `json:"owner,omitempty"`
}

type monitoringFingerprint struct {
	Enabled           bool   `json:"enabled"`
	Interval          string `json:"interval,omitempty"`
	WebhookConfigured bool   `json:"webhookConfigured,omitempty"`
	CheckType         string `json:"checkType,omitempty"`
}

type backupFingerprint struct {
	Schedule string                    `json:"schedule,omitempty"`
	Retain   int                       `json:"retain,omitempty"`
	Volumes  []string                  `json:"volumes,omitempty"`
	Storage  *backupStorageFingerprint `json:"storage,omitempty"`
}

type backupStorageFingerprint struct {
	Provider                  string `json:"provider,omitempty"`
	Bucket                    string `json:"bucket,omitempty"`
	Region                    string `json:"region,omitempty"`
	Endpoint                  string `json:"endpoint,omitempty"`
	Prefix                    string `json:"prefix,omitempty"`
	AccessKeyIDConfigured     bool   `json:"accessKeyIdConfigured,omitempty"`
	SecretAccessKeyConfigured bool   `json:"secretAccessKeyConfigured,omitempty"`
	SessionTokenConfigured    bool   `json:"sessionTokenConfigured,omitempty"`
	ForcePathStyle            bool   `json:"forcePathStyle,omitempty"`
}

func SafeServiceConfigHash(service config.ServiceConfig) (string, bool) {
	fingerprint := safeServiceConfigFingerprint{
		Kind:             service.Kind,
		Schedule:         service.Schedule,
		Timezone:         service.Timezone,
		Timeout:          service.Timeout,
		Build:            service.Build,
		BuildArgs:        cloneStringMap(service.BuildArgs),
		BuildTarget:      service.BuildTarget,
		Dockerfile:       service.Dockerfile,
		Image:            service.Image,
		ImageFrom:        service.ImageFrom,
		Port:             service.Port,
		Ports:            sortedStrings(service.Ports),
		Command:          stringOrListFingerprint(service.Command),
		Entrypoint:       stringOrListFingerprint(service.Entrypoint),
		Labels:           cloneStringMap(service.Labels),
		Replicas:         service.Replicas,
		Restart:          service.Restart,
		EnvKeys:          sortedMapKeys(service.Env),
		EnvFile:          service.EnvFile,
		EnvFiles:         append([]string(nil), service.EnvFiles...),
		RunInputHash:     service.RunInputHash,
		User:             service.User,
		WorkingDir:       service.WorkingDir,
		StopGracePeriod:  service.StopGracePeriod,
		Init:             service.Init,
		ExtraHosts:       sortedStrings(service.ExtraHosts),
		Ulimits:          cloneUlimits(service.Ulimits),
		ShmSize:          service.ShmSize,
		Secrets:          sortedStrings(service.Secrets),
		Volumes:          sortedStrings(service.Volumes),
		Files:            serviceFilesFingerprint(service.Files),
		FilesContentHash: service.FilesContentHash,
		Persistent:       service.Persistent,
		Proxy:            service.Proxy,
		LoadBalancer:     service.LoadBalancer,
		HealthCheck:      service.HealthCheck,
		Deploy:           service.Deploy,
		Backup:           cloneBackupFingerprint(service.Backup),
		Monitoring:       cloneMonitoringFingerprint(service.Monitoring),
		Export:           service.Export,
		Imports:          sortedStrings(service.Imports),
		Placement:        clonePlacement(service.Placement),
		DependsOn:        sortedStrings(service.DependsOn),
		Resources:        cloneResourcesFingerprint(service.Resources),
	}
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), true
}

func serviceFilesFingerprint(files []config.ServiceFileConfig) []serviceFileFingerprint {
	if len(files) == 0 {
		return nil
	}
	out := make([]serviceFileFingerprint, 0, len(files))
	for _, file := range files {
		out = append(out, serviceFileFingerprint{Target: file.Target, Secret: file.Secret, Owner: file.Owner})
	}
	return out
}

func cloneUlimits(source map[string]config.UlimitConfig) map[string]config.UlimitConfig {
	if len(source) == 0 {
		return nil
	}
	copy := make(map[string]config.UlimitConfig, len(source))
	for name, limit := range source {
		copy[name] = limit
	}
	return copy
}

func stringOrListFingerprint(value config.StringOrList) any {
	if !value.IsSet() {
		return nil
	}
	if scalar, ok := value.Scalar(); ok {
		return scalar
	}
	return value.Arguments()
}

func cloneBackupFingerprint(backup *config.BackupConfig) *backupFingerprint {
	if backup == nil {
		return nil
	}
	return &backupFingerprint{
		Schedule: backup.Schedule,
		Retain:   backup.Retain,
		Volumes:  sortedStrings(backup.Volumes),
		Storage:  cloneBackupStorageFingerprint(backup.Storage),
	}
}

func cloneBackupStorageFingerprint(storage *config.BackupStorageConfig) *backupStorageFingerprint {
	if storage == nil {
		return nil
	}
	return &backupStorageFingerprint{
		Provider:                  storage.Provider,
		Bucket:                    storage.Bucket,
		Region:                    storage.Region,
		Endpoint:                  storage.Endpoint,
		Prefix:                    storage.Prefix,
		AccessKeyIDConfigured:     strings.TrimSpace(storage.AccessKeyID) != "",
		SecretAccessKeyConfigured: strings.TrimSpace(storage.SecretAccessKey) != "",
		SessionTokenConfigured:    strings.TrimSpace(storage.SessionToken) != "",
		ForcePathStyle:            storage.ForcePathStyle,
	}
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

func cloneResourcesFingerprint(resources *config.ResourceLimitsConfig) *config.ResourceLimitsConfig {
	if resources == nil || (resources.Memory == "" && resources.CPUs == "") {
		return nil
	}
	clone := *resources
	return &clone
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
