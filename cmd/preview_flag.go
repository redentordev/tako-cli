package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

// previewFlag is the persistent --preview <name> flag.
var previewFlag string

// previewSupportedAnnotation marks the commands that accept --preview
// (deploy, ps, logs, exec, history, rollback, config explain). Every other
// command rejects the flag with exit 2.
const previewSupportedAnnotation = "tako.previewSupported"

func markPreviewSupported(cmds ...*cobra.Command) {
	for _, command := range cmds {
		if command.Annotations == nil {
			command.Annotations = map[string]string{}
		}
		command.Annotations[previewSupportedAnnotation] = "true"
	}
}

// previewRequested reports whether --preview was given.
func previewRequested() bool {
	return previewFlag != ""
}

// rejectUnsupportedPreview fails with an invalid-request error when
// --preview targets a command that does not support it, or names a preview
// that normalizes to nothing.
func rejectUnsupportedPreview(cmd *cobra.Command) error {
	if cmd == nil || !previewRequested() {
		return nil
	}
	if cmd.Annotations[previewSupportedAnnotation] != "true" {
		return &engine.InvalidRequestError{Err: fmt.Errorf("--preview is not supported by %s", cmd.CommandPath())}
	}
	if _, err := config.NormalizePreviewName(previewFlag); err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	return nil
}

// loadPreviewInstanceConfig loads configPath for preview name created from
// base (empty base selects the only preview base, D3). A non-empty
// environment turns on environment-scoped ${VAR} expansion bounded by the
// preview's base: unset references used only by other environments, or by
// servers the base cannot use, do not fail the load. An empty environment
// requires every reference.
func loadPreviewInstanceConfig(configPath string, base string, name string, environment string) (*config.Config, error) {
	return config.LoadConfigWithOptions(configPath, config.LoadOptions{
		Preview:     &config.PreviewInstance{Base: base, Name: name},
		Environment: environment,
	})
}

// previewInstanceLoader is the engine loader for previews other than the
// one this command targets. Each instance is scoped to its own base.
func previewInstanceLoader(configPath string) engine.PreviewInstanceLoader {
	return func(base string, name string) (*config.Config, error) {
		return loadPreviewInstanceConfig(configPath, base, name, base)
	}
}

// findPreviewRecord is a seam for tests.
var findPreviewRecord = func(ctx context.Context, cfg *config.Config, name string) (*engine.PreviewListItem, error) {
	return cliEngine().FindPreviewRecord(ctx, cfg, name)
}

// loadCommandConfig loads the config for a command that accepts --preview.
// Without --preview it is config.LoadConfig. With --preview it reads the
// preview record first (exit 2 when none exists or -e differs from the
// record's base) and loads the instance from the record's base (D10). With
// -e both loads are scoped to that environment, the preview's base.
func loadCommandConfig(ctx context.Context, configPath string) (*config.Config, error) {
	if !previewRequested() {
		return config.LoadConfig(configPath)
	}
	name, err := config.NormalizePreviewName(previewFlag)
	if err != nil {
		return nil, &engine.InvalidRequestError{Err: err}
	}
	plain, err := config.LoadConfigForEnvironment(configPath, envFlag)
	if err != nil {
		return nil, err
	}
	record, err := findPreviewRecord(ctx, plain, name)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, &engine.InvalidRequestError{Err: fmt.Errorf("preview %s not found; deploy it first with tako deploy --preview %s", name, name)}
	}
	if envFlag != "" && envFlag != record.Base {
		return nil, &engine.InvalidRequestError{Err: fmt.Errorf("preview %s was created from %s, not %s", name, record.Base, envFlag)}
	}
	cfg, err := loadPreviewInstanceConfig(configPath, record.Base, name, envFlag)
	if err != nil {
		return nil, &engine.InvalidRequestError{Err: err}
	}
	summaryPreview := engine.SummaryPreview{Name: record.Name, Base: record.Base}
	if record.ExpiresAt != nil {
		summaryPreview.ExpiresAt = record.ExpiresAt.UTC().Format(time.RFC3339)
	}
	cliEngine().RegisterPreviewEnvironment(record.Environment, summaryPreview)
	return cfg, nil
}

// loadCommandConfigOrError is loadCommandConfig with the located config
// errors of formatConfigLoadError. Classified errors (invalid request,
// connectivity, attention) keep their exit code.
func loadCommandConfigOrError(ctx context.Context, configPath string) (*config.Config, error) {
	cfg, err := loadCommandConfig(ctx, configPath)
	if err == nil {
		return cfg, nil
	}
	if engine.Classify(err) != engine.ClassFailed {
		return nil, err
	}
	return nil, formatConfigLoadError(configPath, err)
}

// loadOfflinePreviewConfig loads the instance --preview names without
// reading any record: the base is -e or the only preview base (D3). Every
// ${VAR} reference is required.
func loadOfflinePreviewConfig(configPath string) (*config.Config, error) {
	return loadOfflinePreviewConfigForEnvironment(configPath, "")
}

// loadOfflinePreviewConfigForEnvironment is loadOfflinePreviewConfig with
// environment-scoped ${VAR} expansion when environment (-e) is set; with
// --preview the scope is the preview's base, which -e names. Used by deploy
// (which checks the record in the engine), validate and config explain.
func loadOfflinePreviewConfigForEnvironment(configPath string, environment string) (*config.Config, error) {
	if !previewRequested() {
		return config.LoadConfigForEnvironment(configPath, environment)
	}
	cfg, err := loadPreviewInstanceConfig(configPath, envFlag, previewFlag, environment)
	if err != nil {
		return nil, &engine.InvalidRequestError{Err: err}
	}
	if instance := cfg.PreviewInstance(); instance != nil {
		cliEngine().RegisterPreviewEnvironment(instance.Identity, engine.SummaryPreview{Name: instance.Name, Base: instance.Base})
	}
	return cfg, nil
}
