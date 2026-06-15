package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/secrets"
)

func buildOperatorEnvFileContent(environment string, service *config.ServiceConfig) (string, error) {
	hasEnvVars := len(service.Env) > 0 || len(service.Secrets) > 0 || service.EnvFile != ""
	if !hasEnvVars {
		return "", nil
	}

	secretsMgr, err := secrets.NewManager(environment)
	if err != nil {
		return "", fmt.Errorf("failed to create secrets manager: %w", err)
	}
	envFile, err := secretsMgr.CreateEnvFile(service)
	if err != nil {
		return "", fmt.Errorf("failed to create env file: %w", err)
	}
	data, err := io.ReadAll(envFile.ToReader())
	if err != nil {
		return "", fmt.Errorf("failed to read env file: %w", err)
	}
	return string(data), nil
}

func buildOperatorMountSpecs(cfg *config.Config, environment string, serviceName string, service *config.ServiceConfig) ([]string, error) {
	var mounts []string
	for _, volume := range service.Volumes {
		if config.IsNFSVolume(volume) {
			return nil, fmt.Errorf("service %s: NFS volumes are no longer supported; use node-local volumes or an external storage service", serviceName)
		}
		if err := config.ValidateVolumeMountSpec(volume); err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}

		spec, err := config.ParseVolumeMountSpec(volume)
		if err != nil {
			return nil, fmt.Errorf("service %s: %w", serviceName, err)
		}
		readOnly := operatorMountReadOnlyOption(spec.Mode)
		if !spec.HasTarget {
			target := spec.Source
			source := operatorDockerVolumeName(cfg, environment, target)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s%s", source, target, readOnly))
			continue
		}

		if spec.Source[0] == '/' {
			mounts = append(mounts, fmt.Sprintf("type=bind,source=%s,target=%s%s", spec.Source, spec.Target, readOnly))
		} else {
			namedVolume := operatorDockerVolumeName(cfg, environment, spec.Source)
			mounts = append(mounts, fmt.Sprintf("type=volume,source=%s,target=%s%s", namedVolume, spec.Target, readOnly))
		}
	}
	return mounts, nil
}

func operatorDockerVolumeName(cfg *config.Config, environment string, logicalName string) string {
	if strings.HasPrefix(logicalName, "/") {
		return runtimeid.VolumeName(cfg.Project.Name, environment, logicalName)
	}
	return cfg.GetVolumeName(logicalName, environment)
}

func operatorMountReadOnlyOption(mode string) string {
	if mode == "ro" {
		return ",readonly"
	}
	return ""
}
