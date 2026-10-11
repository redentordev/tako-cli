package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Local lifecycle hook defaults and bounds; see docs/HOOKS.md.
const (
	// DefaultHooksPath is the hook directory, relative to the directory
	// that contains the loaded config file. It lives outside .tako/ because
	// `tako init` gitignores .tako/ and hooks must be committable.
	DefaultHooksPath = ".tako-hooks"
	// DefaultHookTimeout bounds each hook run unless hooks.timeout is set.
	DefaultHookTimeout = 5 * time.Minute
	MinHookTimeout     = time.Second
	MaxHookTimeout     = time.Hour

	HookOutputVerbose = "verbose"
	HookOutputQuiet   = "quiet"
)

// HookNames lists every lifecycle hook Tako recognizes, in lifecycle order.
// pkg/hooks exposes the same list as typed names.
var HookNames = []string{
	"pre-deploy",
	"post-deploy",
	"pre-rollback",
	"post-rollback",
	"pre-promote",
	"post-promote",
	"pre-setup",
	"post-setup",
	"on-failure",
}

// IsHookName reports whether name is a recognized lifecycle hook.
func IsHookName(name string) bool {
	return slices.Contains(HookNames, name)
}

var hookPassEnvPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\*?$`)

// HooksConfig configures local lifecycle hooks: executables in the hook
// directory or argv commands that run on the operator's machine around
// deploy, rollback, promote, and setup. Hooks never run on nodes.
type HooksConfig struct {
	// Path is the hook directory relative to the config file's directory
	// (default .tako-hooks). It must stay inside the project and outside
	// the gitignored .tako/ directory.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
	// Timeout bounds each hook run as a Go duration from 1s to 1h
	// (default 5m).
	Timeout string `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// Output is verbose (hook lines at info level, the default) or quiet
	// (debug level, shown only with --verbose or in NDJSON).
	Output string `yaml:"output,omitempty" json:"output,omitempty"`
	// PassEnv forwards process variables to hooks by exact name or a
	// PREFIX* pattern. Nothing else from the process environment, including
	// .env values, reaches a hook.
	PassEnv []string `yaml:"passEnv,omitempty" json:"passEnv,omitempty"`
	// Commands declares hooks as argv lists run without a shell. ${...}
	// references are rejected; use PassEnv instead.
	Commands map[string][]string `yaml:"commands,omitempty" json:"commands,omitempty"`
}

// HooksDir returns the configured hook directory (slash-separated, relative
// to the config directory), or the default.
func (h *HooksConfig) HooksDir() string {
	if h == nil || strings.TrimSpace(h.Path) == "" {
		return DefaultHooksPath
	}
	return path.Clean(filepath.ToSlash(strings.TrimSpace(h.Path)))
}

// TimeoutDuration returns the per-hook timeout, or the default when unset
// or unparseable (validation rejects unparseable values at load time).
func (h *HooksConfig) TimeoutDuration() time.Duration {
	if h == nil || strings.TrimSpace(h.Timeout) == "" {
		return DefaultHookTimeout
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(h.Timeout))
	if err != nil {
		return DefaultHookTimeout
	}
	return parsed
}

// OutputMode returns verbose or quiet.
func (h *HooksConfig) OutputMode() string {
	if h == nil || strings.TrimSpace(h.Output) == "" {
		return HookOutputVerbose
	}
	return strings.TrimSpace(h.Output)
}

// IsCredentialEnvName reports whether a passEnv variable name looks like a
// credential, so its value is registered with the output redactor.
func IsCredentialEnvName(name string) bool {
	upper := strings.ToUpper(name)
	for _, marker := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "PASS", "CREDENTIAL", "AUTH"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

// validateHooks validates the expanded hooks block.
func validateHooks(hooks *HooksConfig) error {
	if hooks == nil {
		return nil
	}
	if err := validateHooksPath(hooks.Path); err != nil {
		return err
	}
	if timeout := strings.TrimSpace(hooks.Timeout); timeout != "" {
		parsed, err := time.ParseDuration(timeout)
		if err != nil {
			return fmt.Errorf("hooks.timeout: %q is not a valid duration (examples: 30s, 5m, 1h)", hooks.Timeout)
		}
		if parsed < MinHookTimeout || parsed > MaxHookTimeout {
			return fmt.Errorf("hooks.timeout: %s is out of range; must be between 1s and 1h", timeout)
		}
	}
	switch strings.TrimSpace(hooks.Output) {
	case "", HookOutputVerbose, HookOutputQuiet:
	default:
		return fmt.Errorf("hooks.output: %q is not supported; use verbose or quiet", hooks.Output)
	}
	for i, name := range hooks.PassEnv {
		if !hookPassEnvPattern.MatchString(name) {
			return fmt.Errorf("hooks.passEnv[%d]: %q must be a variable name or a NAME_PREFIX* pattern", i, name)
		}
		if strings.HasPrefix(strings.ToUpper(name), "TAKO_") {
			return fmt.Errorf("hooks.passEnv[%d]: %q is not allowed; Tako owns the TAKO_ namespace and sets hook context variables itself", i, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(hooks.Commands)) {
		if !IsHookName(name) {
			return fmt.Errorf("hooks.commands.%s: unknown hook name; valid hooks are %s", name, strings.Join(HookNames, ", "))
		}
		argv := hooks.Commands[name]
		if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
			return fmt.Errorf("hooks.commands.%s: command must be a non-empty argv list, e.g. [\"make\", \"check\"]", name)
		}
		for _, arg := range argv {
			if hasConfigControlChars(arg) {
				return fmt.Errorf("hooks.commands.%s: arguments must not contain control characters", name)
			}
		}
	}
	return nil
}

func validateHooksPath(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	slashed := filepath.ToSlash(trimmed)
	if filepath.IsAbs(trimmed) || strings.HasPrefix(slashed, "/") || filepath.VolumeName(trimmed) != "" || windowsDrivePattern.MatchString(slashed) {
		return fmt.Errorf("hooks.path: %q must be relative to the directory that contains the config file", raw)
	}
	cleaned := path.Clean(strings.ReplaceAll(trimmed, `\`, "/"))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return fmt.Errorf("hooks.path: %q escapes the project directory; hooks must live inside the project", raw)
	}
	if cleaned == ".tako" || strings.HasPrefix(cleaned, ".tako/") {
		return fmt.Errorf("hooks.path: %q is inside .tako/, which `tako init` adds to .gitignore, so hooks there cannot be committed; use the default %s", raw, DefaultHooksPath)
	}
	return nil
}

var windowsDrivePattern = regexp.MustCompile(`^[A-Za-z]:`)

// rawHooksDocument is the pre-expansion shadow of hooks.commands.
type rawHooksDocument struct {
	Hooks struct {
		Commands map[string]any `yaml:"commands" json:"commands"`
	} `yaml:"hooks" json:"hooks"`
}

// validateRawHookCommands rejects ${...} references inside hooks.commands
// before env expansion. Hook argv appears in events and process listings,
// so secrets must reach hooks through hooks.passEnv instead.
func validateRawHookCommands(data []byte, isJSON bool) error {
	var doc rawHooksDocument
	if isJSON {
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil // the strict parse after expansion reports the real error
		}
	} else {
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return nil
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.Hooks.Commands)) {
		if rawAliasValueHasEnvRef(doc.Hooks.Commands[name]) {
			return fmt.Errorf("hooks.commands.%s: ${...} references are not allowed in hook commands because argv is visible in events and process listings; list the variable in hooks.passEnv and read it inside the hook", name)
		}
	}
	return nil
}
