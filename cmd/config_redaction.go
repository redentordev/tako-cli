package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	takoconfig "github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/hooks"
	"github.com/redentordev/tako-cli/pkg/secrets"
)

// configExplainRedaction selects the redaction report in `tako config
// explain --redaction`.
var configExplainRedaction bool

// redactionReportLine is one rendered row: a label and why it is masked or
// visible. Values are never part of a row.
type redactionReportLine struct {
	label  string
	reason string
	masked bool
}

// runConfigRedactionReport prints what Tako masks for the selected
// environment and why. It reads the config, local envFiles and local
// .tako/secrets* key names only: it never prints a value, never contacts a
// server and never invokes a secret provider CLI.
func runConfigRedactionReport(out io.Writer, cfg *takoconfig.Config, configPath string, envName string) error {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}
	serverNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		return formatDeployConfigError(configPath, fmt.Errorf("invalid config: %w", err))
	}
	configDir := filepath.Dir(configPath)

	fmt.Fprintf(out, "Redaction: %s (config %s)\n", envName, filepath.Clean(configPath))
	if secrets.RedactionLabelsEnabled() {
		fmt.Fprintf(out, "Labels: on (%s)\n\n", secrets.RedactionLabelsEnv)
	} else {
		fmt.Fprintf(out, "Labels: off (%s=%s); every mask renders as [REDACTED]\n\n", secrets.RedactionLabelsEnv, os.Getenv(secrets.RedactionLabelsEnv))
	}

	// Config-level credentials, registered by every command that registers
	// config secrets; build args are registered by deploy.
	var masked []redactionReportLine
	for _, entry := range secrets.ClassifyConfigCredentials(cfg, services) {
		masked = append(masked, redactionReportLine{label: entry.Label, reason: entry.Reason.String(), masked: true})
	}
	for _, entry := range secrets.ClassifyBuildArgs(cfg) {
		masked = append(masked, redactionReportLine{label: entry.Label, reason: entry.Reason.String() + " (deploy)", masked: true})
	}
	var passEnv []string
	if cfg.Hooks != nil {
		passEnv = cfg.Hooks.PassEnv
	}
	for _, credential := range hooks.CredentialEnv(os.Environ(), passEnv, runtime.GOOS) {
		masked = append(masked, redactionReportLine{label: secrets.HookPassEnvLabel(credential.Name), reason: secrets.ReasonPassEnvCredentialName.String() + " (when set on this machine)", masked: true})
	}
	fmt.Fprintln(out, "Masked")
	if len(masked) == 0 {
		fmt.Fprintln(out, "  (no config-level credentials)")
	}
	writeRedactionRows(out, "  ", masked, false)
	fmt.Fprintln(out)

	// Service-scoped entries.
	localKeys, localFiles := secrets.LocalSecretKeys(".tako", envName)
	sources := cfg.SecretSourcesFor(envName)
	envEntries := secrets.ClassifyServiceEnv(services)
	fileEntries, fileFailures := secrets.ClassifyServiceEnvFiles(services)
	unreadableFiles := make(map[string]bool, len(fileFailures))
	for _, failure := range fileFailures {
		unreadableFiles[failure.Service] = true
	}
	type unresolved struct{ label, service string }
	var notResolvable []unresolved

	serviceNames := make([]string, 0, len(services))
	for name := range services {
		serviceNames = append(serviceNames, name)
	}
	sort.Strings(serviceNames)

	fmt.Fprintln(out, "Services")
	for _, serviceName := range serviceNames {
		service := services[serviceName]
		var rows []redactionReportLine

		seen := make(map[string]bool)
		for _, ref := range service.Secrets {
			key := secrets.SecretRefKey(ref)
			if seen[key] {
				continue
			}
			seen[key] = true
			label := secrets.SecretLabel(key)
			reason := secrets.ReasonSecretsList.String()
			if file, ok := localKeys[key]; ok {
				reason += " (from " + file + ")"
			} else if index := secrets.SecretSourceIndexFor(sources, key); index >= 0 {
				reason += fmt.Sprintf(" (from secretSources[%d] %s; not resolved here)", index, sources[index].Provider)
			} else {
				notResolvable = append(notResolvable, unresolved{label: label, service: serviceName})
				continue
			}
			rows = append(rows, redactionReportLine{label: label, reason: reason, masked: true})
		}
		for _, entry := range envEntries {
			if entry.Service == serviceName {
				rows = append(rows, redactionReportLine{label: entry.Label, reason: envReasonText(entry), masked: entry.Masked})
			}
		}
		for _, entry := range fileEntries {
			if entry.Service == serviceName {
				reason := envReasonText(entry)
				if entry.Note != "" {
					reason += " (" + displayConfigRelativePath(configDir, entry.Note) + ")"
				}
				rows = append(rows, redactionReportLine{label: entry.Label, reason: reason, masked: entry.Masked})
			}
		}

		fmt.Fprintf(out, "  %s:\n", serviceName)
		if len(rows) == 0 && !unreadableFiles[serviceName] {
			fmt.Fprintln(out, "    (no secrets, env or envFile values)")
			continue
		}
		writeRedactionRows(out, "    ", rows, true)
		if unreadableFiles[serviceName] {
			var paths []string
			for _, path := range secrets.ServiceEnvFilePaths(&service) {
				paths = append(paths, displayConfigRelativePath(configDir, path))
			}
			fmt.Fprintf(out, "    envFile unreadable: %s; its values will not be redacted\n", strings.Join(paths, ", "))
		}
	}
	fmt.Fprintln(out)

	if len(notResolvable) > 0 {
		fmt.Fprintln(out, "Not resolvable")
		for _, entry := range notResolvable {
			fmt.Fprintf(out, "  %s (%s)  not in .tako/secrets* and no secretSources; will not be redacted\n", entry.label, entry.service)
		}
		fmt.Fprintln(out)
	}
	for _, file := range localFiles {
		if file.Err != nil {
			fmt.Fprintf(out, "Note: could not read %s; its keys are treated as missing\n\n", file.Name)
		}
	}
	if len(sources) > 0 {
		fmt.Fprintln(out, "Note: secretSources are not resolved here; sourced keys are verified at deploy/logs time (preview with `tako secrets fetch`).")
		fmt.Fprintln(out)
	}

	environments := make([]string, 0, len(cfg.Environments))
	for name := range cfg.Environments {
		environments = append(environments, name)
	}
	sort.Strings(environments)
	fmt.Fprintf(out, "Always visible: project %s; environments %s; servers %s; service names; service images\n",
		cfg.Project.Name, strings.Join(environments, ", "), strings.Join(serverNames, ", "))
	fmt.Fprintf(out, "Patterns: %s (heuristic, labeled pattern.<id>)\n", strings.Join(secrets.PatternIDs(), ", "))
	return nil
}

func envReasonText(entry secrets.RedactionEntry) string {
	reason := entry.Reason.String()
	if entry.Reason == secrets.ReasonPlainSetting && strings.Contains(entry.Value, "://") {
		reason += " (URL without credentials)"
	}
	return reason
}

func writeRedactionRows(out io.Writer, indent string, rows []redactionReportLine, withState bool) {
	if withState {
		// Masked rows first; each group keeps its order (secrets, env,
		// envFile, each sorted by label).
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].masked && !rows[j].masked })
	}
	width := 0
	for _, row := range rows {
		if len(row.label) > width {
			width = len(row.label)
		}
	}
	for _, row := range rows {
		if withState {
			state := "visible"
			if row.masked {
				state = "masked"
			}
			fmt.Fprintf(out, "%s%-8s %-*s  %s\n", indent, state, width, row.label, row.reason)
			continue
		}
		fmt.Fprintf(out, "%s%-*s  %s\n", indent, width, row.label, row.reason)
	}
}

// displayConfigRelativePath shows a config-resolved path relative to the
// config directory when it lies beneath it.
func displayConfigRelativePath(configDir, path string) string {
	if absDir, err := filepath.Abs(configDir); err == nil {
		if rel, err := filepath.Rel(absDir, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(path)
}
