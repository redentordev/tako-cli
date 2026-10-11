package secrets

import (
	"strings"
)

// Redaction labels name where a masked value came from. A marker renders as
// `[REDACTED:<label>]`; a label is config-path-like and starts with a fixed
// kind. Labels are built only from config names (secret keys, env keys,
// server, registry, environment and service names) and fixed vocabulary,
// never from a value or a secretSources locator.
const (
	LabelKindSecrets       = "secrets"
	LabelKindEnvFile       = "envFile"
	LabelKindEnv           = "env"
	LabelKindRegistries    = "registries"
	LabelKindServers       = "servers"
	LabelKindEnvironments  = "environments"
	LabelKindServices      = "services"
	LabelKindBuilds        = "builds"
	LabelKindNotifications = "notifications"
	LabelKindHooks         = "hooks"
	LabelKindRun           = "run"
	LabelKindExport        = "export"
	LabelKindCerts         = "certs"
	LabelKindProxy         = "proxy"
	LabelKindPattern       = "pattern"

	// MaxLabelLength caps a label in bytes.
	MaxLabelLength = 96
)

// labelPrecedence orders label kinds, highest first. When one value is
// registered under several labels, the label of the highest kind wins, with
// ties broken lexicographically; unlabeled registrations rank last.
var labelPrecedence = []string{
	LabelKindSecrets,
	LabelKindEnvFile,
	LabelKindEnv,
	LabelKindRegistries,
	LabelKindServers,
	LabelKindEnvironments,
	LabelKindServices,
	LabelKindBuilds,
	LabelKindNotifications,
	LabelKindHooks,
	LabelKindRun,
	LabelKindExport,
	LabelKindCerts,
	LabelKindProxy,
}

// labelRank returns the precedence rank of a label; lower ranks win.
func labelRank(label string) int {
	if label == "" {
		return len(labelPrecedence) + 1
	}
	kind, _, _ := strings.Cut(label, ".")
	for rank, candidate := range labelPrecedence {
		if kind == candidate {
			return rank
		}
	}
	return len(labelPrecedence)
}

// preferLabel reports whether candidate should replace current as the
// rendered label of a value.
func preferLabel(candidate, current string) bool {
	candidateRank, currentRank := labelRank(candidate), labelRank(current)
	if candidateRank != currentRank {
		return candidateRank < currentRank
	}
	return candidate < current
}

func isLabelChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '-' || c == '@' || c == '/' || c == ':':
		return true
	}
	return false
}

// SanitizeLabel forces a label into the marker grammar: characters outside
// `[A-Za-z0-9_\-@/:]` and the `.` separator become `_`, empty segments become
// `_`, and labels longer than MaxLabelLength bytes are truncated from the
// end, which keeps the kind prefix.
func SanitizeLabel(label string) string {
	if label == "" {
		return ""
	}
	segments := strings.Split(label, ".")
	for i, segment := range segments {
		if segment == "" {
			segments[i] = "_"
			continue
		}
		b := []byte(segment)
		for j := range b {
			if !isLabelChar(b[j]) {
				b[j] = '_'
			}
		}
		segments[i] = string(b)
	}
	out := strings.Join(segments, ".")
	if len(out) > MaxLabelLength {
		out = strings.TrimRight(out[:MaxLabelLength], ".")
	}
	return out
}

// ValidLabel reports whether label matches the marker grammar.
func ValidLabel(label string) bool {
	if label == "" || len(label) > MaxLabelLength {
		return false
	}
	for _, segment := range strings.Split(label, ".") {
		if segment == "" {
			return false
		}
		for i := 0; i < len(segment); i++ {
			if !isLabelChar(segment[i]) {
				return false
			}
		}
	}
	return true
}

func buildLabel(parts ...string) string {
	return SanitizeLabel(strings.Join(parts, "."))
}

// SecretLabel labels a value referenced by a `secrets:` list, from local
// .tako/secrets* files or secretSources: `secrets.<KEY>`.
func SecretLabel(key string) string { return buildLabel(LabelKindSecrets, key) }

// EnvLabel labels a credential-like inline `env:` value: `env.<NAME>`.
func EnvLabel(name string) string { return buildLabel(LabelKindEnv, name) }

// EnvFileLabel labels a credential-like envFile value: `envFile.<NAME>`.
func EnvFileLabel(name string) string { return buildLabel(LabelKindEnvFile, name) }

// ServerPasswordLabel labels an SSH password: `servers.<server>.password`.
func ServerPasswordLabel(server string) string {
	return buildLabel(LabelKindServers, server, "password")
}

// RegistryPasswordLabel labels a registry password:
// `registries.<host>.password`.
func RegistryPasswordLabel(host string) string {
	return buildLabel(LabelKindRegistries, host, "password")
}

// RegistryTokenLabel labels a registry token minted by a
// `registries.<host>.provider`: `registries.<host>.token`.
func RegistryTokenLabel(host string) string {
	return buildLabel(LabelKindRegistries, host, "token")
}

// ACMECredentialLabel labels an ACME DNS provider credential:
// `environments.<env>.proxy.acme.<NAME>`.
func ACMECredentialLabel(environment, name string) string {
	return buildLabel(LabelKindEnvironments, environment, "proxy", "acme", name)
}

// BackupCredentialLabel labels a backup storage credential:
// `services.<service>.backup.<field>` (accessKeyId or secretAccessKey).
func BackupCredentialLabel(service, field string) string {
	return buildLabel(LabelKindServices, service, "backup", field)
}

// BuildArgLabel labels a shared build arg value:
// `builds.<build>.args.<ARG>`.
func BuildArgLabel(build, arg string) string {
	return buildLabel(LabelKindBuilds, build, "args", arg)
}

// NotificationLabel labels a notification URL: `notifications.<kind>`
// (slack, discord or webhook).
func NotificationLabel(kind string) string { return buildLabel(LabelKindNotifications, kind) }

// HookPassEnvLabel labels a credential passed to hooks through passEnv:
// `hooks.passEnv.<NAME>`.
func HookPassEnvLabel(name string) string { return buildLabel(LabelKindHooks, "passEnv", name) }

// RunEnvLabel labels a `tako run` request env value: `run.env.<NAME>`.
func RunEnvLabel(name string) string { return buildLabel(LabelKindRun, "env", name) }

// ExportPasswordLabel labels the `config export` password.
func ExportPasswordLabel() string { return LabelKindExport + ".password" }

// CertsPrivateKeyLabel labels a generated TLS private key.
func CertsPrivateKeyLabel() string { return LabelKindCerts + ".privateKey" }

// ProxyPasswordLabel labels the `proxy hash-password` input.
func ProxyPasswordLabel() string { return LabelKindProxy + ".password" }

// PatternLabel labels a heuristic pattern match: `pattern.<id>`.
func PatternLabel(id string) string { return buildLabel(LabelKindPattern, id) }

// RedactionReason explains why a value is masked or stays visible.
type RedactionReason int

const (
	ReasonNone RedactionReason = iota
	ReasonSecretsList
	ReasonCredentialName
	ReasonURLWithCredentials
	ReasonTokenURLPart
	ReasonTokenShapedValue
	ReasonSSHPassword
	ReasonRegistryCredential
	ReasonRegistryProviderToken
	ReasonDNSProviderCredential
	ReasonBackupStorageCredential
	ReasonBuildArg
	ReasonNotificationURL
	ReasonPassEnvCredentialName
	ReasonPlainSetting
	ReasonEmpty
)

// String returns the display text used by `tako config explain --redaction`.
func (r RedactionReason) String() string {
	switch r {
	case ReasonSecretsList:
		return "secrets list"
	case ReasonCredentialName:
		return "credential-like name"
	case ReasonURLWithCredentials:
		return "URL with credentials"
	case ReasonTokenURLPart:
		return "token-like URL part"
	case ReasonTokenShapedValue:
		return "token-shaped value"
	case ReasonSSHPassword:
		return "ssh password"
	case ReasonRegistryCredential:
		return "registry credential"
	case ReasonRegistryProviderToken:
		return "registry token (provider)"
	case ReasonDNSProviderCredential:
		return "DNS provider credential"
	case ReasonBackupStorageCredential:
		return "backup storage credential"
	case ReasonBuildArg:
		return "build arg"
	case ReasonNotificationURL:
		return "notification URL"
	case ReasonPassEnvCredentialName:
		return "passEnv credential name"
	case ReasonPlainSetting:
		return "plain setting"
	case ReasonEmpty:
		return "empty"
	}
	return ""
}

// Masked reports whether values with this reason are registered.
func (r RedactionReason) Masked() bool {
	switch r {
	case ReasonNone, ReasonPlainSetting, ReasonEmpty:
		return false
	}
	return true
}
