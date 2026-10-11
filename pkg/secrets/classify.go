package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/redentordev/tako-cli/pkg/config"
)

// RedactionEntry describes one value Tako masks, or one env key it leaves
// visible, and why. The engine registers the Masked entries, and
// `tako config explain --redaction` renders them without values, so the
// report and the runtime registration come from the same classification.
type RedactionEntry struct {
	// Label is the marker label (`env.API_TOKEN`); for a visible entry it is
	// the label the value would carry if it were masked.
	Label  string
	Reason RedactionReason
	Masked bool
	// Service is the owning service for service-scoped entries.
	Service string
	// Note adds context for the report, such as the envFile path.
	Note string
	// Value is the classified value. It is registered with the redactor and
	// must never be printed.
	Value string
}

func sortEntries(entries []RedactionEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Service != entries[j].Service {
			return entries[i].Service < entries[j].Service
		}
		return entries[i].Label < entries[j].Label
	})
}

func maskedEntry(label string, reason RedactionReason, value string) RedactionEntry {
	return RedactionEntry{Label: label, Reason: reason, Masked: true, Value: value}
}

// ClassifyServerPasswords returns an entry for every server with an SSH
// password.
func ClassifyServerPasswords(servers map[string]config.ServerConfig) []RedactionEntry {
	var entries []RedactionEntry
	for name, server := range servers {
		if server.Password != "" {
			entries = append(entries, maskedEntry(ServerPasswordLabel(name), ReasonSSHPassword, server.Password))
		}
	}
	sortEntries(entries)
	return entries
}

// ClassifyRegistryCredentials returns an entry for every registry password.
func ClassifyRegistryCredentials(cfg *config.Config) []RedactionEntry {
	if cfg == nil {
		return nil
	}
	var entries []RedactionEntry
	for host, registry := range cfg.Registries {
		if registry.Password != "" {
			entries = append(entries, maskedEntry(RegistryPasswordLabel(host), ReasonRegistryCredential, registry.Password))
		}
	}
	sortEntries(entries)
	return entries
}

// ClassifyACMEDNSCredentials returns an entry for every environment-scoped
// ACME DNS provider credential.
func ClassifyACMEDNSCredentials(cfg *config.Config) []RedactionEntry {
	if cfg == nil {
		return nil
	}
	var entries []RedactionEntry
	for envName, environment := range cfg.Environments {
		if environment.Proxy == nil || environment.Proxy.ACME == nil {
			continue
		}
		for name, credential := range environment.Proxy.ACME.Credentials {
			if credential != "" {
				entries = append(entries, maskedEntry(ACMECredentialLabel(envName, name), ReasonDNSProviderCredential, credential))
			}
		}
	}
	sortEntries(entries)
	return entries
}

// ClassifyNotificationURLs returns an entry for every notification URL.
func ClassifyNotificationURLs(cfg *config.Config) []RedactionEntry {
	if cfg == nil || cfg.Notifications == nil {
		return nil
	}
	var entries []RedactionEntry
	for _, n := range []struct{ kind, value string }{
		{"slack", cfg.Notifications.Slack},
		{"discord", cfg.Notifications.Discord},
		{"webhook", cfg.Notifications.Webhook},
	} {
		if n.value != "" {
			entries = append(entries, maskedEntry(NotificationLabel(n.kind), ReasonNotificationURL, n.value))
		}
	}
	return entries
}

// ClassifyBuildArgs returns an entry for every shared build arg value. Deploy
// registers all of them, as it always has: build args commonly carry tokens.
func ClassifyBuildArgs(cfg *config.Config) []RedactionEntry {
	if cfg == nil {
		return nil
	}
	var entries []RedactionEntry
	for buildName, build := range cfg.Builds {
		for arg, value := range build.Args {
			if value != "" {
				entries = append(entries, maskedEntry(BuildArgLabel(buildName, arg), ReasonBuildArg, value))
			}
		}
	}
	sortEntries(entries)
	return entries
}

// ClassifyBackupCredentials returns an entry for each service's backup
// storage accessKeyId and secretAccessKey.
func ClassifyBackupCredentials(services map[string]config.ServiceConfig) []RedactionEntry {
	var entries []RedactionEntry
	for name, service := range services {
		if service.Backup == nil || service.Backup.Storage == nil {
			continue
		}
		storage := service.Backup.Storage
		for _, field := range []struct{ name, value string }{
			{"accessKeyId", storage.AccessKeyID},
			{"secretAccessKey", storage.SecretAccessKey},
			{"sessionToken", storage.SessionToken},
		} {
			if field.value != "" {
				entry := maskedEntry(BackupCredentialLabel(name, field.name), ReasonBackupStorageCredential, field.value)
				entry.Service = name
				entries = append(entries, entry)
			}
		}
	}
	sortEntries(entries)
	return entries
}

// ClassifyServiceEnv returns an entry for every inline `env:` value of every
// service, masked or visible (see ClassifyEnvValue).
func ClassifyServiceEnv(services map[string]config.ServiceConfig) []RedactionEntry {
	var entries []RedactionEntry
	for serviceName, service := range services {
		for name, value := range service.Env {
			reason := ClassifyEnvValue(name, value)
			entries = append(entries, RedactionEntry{
				Label:   EnvLabel(name),
				Reason:  reason,
				Masked:  reason.Masked(),
				Service: serviceName,
				Value:   value,
			})
		}
	}
	sortEntries(entries)
	return entries
}

// ServiceEnvFileError reports an envFile that could not be read.
type ServiceEnvFileError struct {
	Service string
	Err     error
}

// ClassifyServiceEnvFiles returns an entry for every envFile/envFiles value of
// every service, masked or visible, plus one error per service whose files
// cannot be read. A service with an unreadable file contributes no entries.
func ClassifyServiceEnvFiles(services map[string]config.ServiceConfig) ([]RedactionEntry, []ServiceEnvFileError) {
	var entries []RedactionEntry
	var failures []ServiceEnvFileError
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, serviceName := range names {
		service := services[serviceName]
		values, sources, err := loadServiceEnvFilesWithSource(&service)
		if err != nil {
			failures = append(failures, ServiceEnvFileError{Service: serviceName, Err: err})
			continue
		}
		for name, value := range values {
			reason := ClassifyEnvValue(name, value)
			entries = append(entries, RedactionEntry{
				Label:   EnvFileLabel(name),
				Reason:  reason,
				Masked:  reason.Masked(),
				Service: serviceName,
				Note:    sources[name],
				Value:   value,
			})
		}
	}
	sortEntries(entries)
	return entries, failures
}

// ClassifyConfigCredentials returns the config-level credentials Tako
// registers in every command that registers config secrets: SSH passwords,
// registry passwords, ACME DNS credentials, backup storage credentials of
// the given services, and notification URLs.
func ClassifyConfigCredentials(cfg *config.Config, services map[string]config.ServiceConfig) []RedactionEntry {
	var entries []RedactionEntry
	if cfg != nil {
		entries = append(entries, ClassifyServerPasswords(cfg.Servers)...)
	}
	entries = append(entries, ClassifyRegistryCredentials(cfg)...)
	entries = append(entries, ClassifyACMEDNSCredentials(cfg)...)
	entries = append(entries, ClassifyBackupCredentials(services)...)
	entries = append(entries, ClassifyNotificationURLs(cfg)...)
	return entries
}

// LocalSecretFile is a local .tako secrets file whose keys were read.
type LocalSecretFile struct {
	// Name is the display path, `.tako/secrets` or `.tako/secrets.<env>`.
	Name string
	// Err is set when the file exists but could not be read.
	Err error
}

// LocalSecretKeys reads the key names of the local secrets files for an
// environment, the same files `tako secrets list` reads: the common file and
// the environment file, which overrides it. It returns, for each key, the
// display name of the file that supplies it. Values are discarded and
// `$(command)` substitutions are never executed. Missing files are skipped;
// unreadable files are reported in files with Err set.
func LocalSecretKeys(basePath, environment string) (map[string]string, []LocalSecretFile) {
	keys := make(map[string]string)
	var files []LocalSecretFile
	names := []string{"secrets"}
	if environment != "" {
		names = append(names, "secrets."+environment)
	}
	for _, name := range names {
		path := filepath.Join(basePath, name)
		if _, err := os.Stat(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				files = append(files, LocalSecretFile{Name: ".tako/" + name, Err: err})
			}
			continue
		}
		values, err := readSecretsFile(path)
		if err != nil {
			files = append(files, LocalSecretFile{Name: ".tako/" + name, Err: fmt.Errorf("could not read %s", ".tako/"+name)})
			continue
		}
		files = append(files, LocalSecretFile{Name: ".tako/" + name})
		for key := range values {
			keys[key] = ".tako/" + name
		}
	}
	return keys, files
}

// SecretSourceIndexFor returns the index of the secretSources entry that
// supplies key, without contacting any provider: the last source whose
// explicit names (after map and prefixStrip) produce key, otherwise the last
// source in listing mode, which may supply any key. It returns -1 when no
// source can supply the key.
func SecretSourceIndexFor(sources []config.SecretSource, key string) int {
	for i := len(sources) - 1; i >= 0; i-- {
		source := sources[i]
		for _, name := range source.Names {
			mapped, ok := source.Map[name]
			if !ok {
				mapped = ProviderSecretKey(name, source.PrefixStrip)
			}
			if mapped == key {
				return i
			}
		}
	}
	for i := len(sources) - 1; i >= 0; i-- {
		if len(sources[i].Names) == 0 {
			return i
		}
	}
	return -1
}
