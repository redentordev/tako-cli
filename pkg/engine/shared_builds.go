package engine

import (
	"fmt"
	"sort"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/deployplan"
)

func buildSharedImages(d *deployer.Deployer, cfg *config.Config, envName string, buildTag string, services map[string]config.ServiceConfig, skipBuild bool) error {
	if skipBuild {
		return ensureSharedImagesWith(cfg, envName, buildTag, services, d.EnsureSharedTakodImage)
	}
	return buildSharedImagesWith(cfg, envName, buildTag, services, d.BuildSharedTakodImage)
}

func ensureSharedImagesWith(cfg *config.Config, envName string, buildTag string, services map[string]config.ServiceConfig, ensure func(string, string, map[string]config.ServiceConfig) error) error {
	consumers := make(map[string]map[string]config.ServiceConfig)
	for serviceName, service := range services {
		if service.SharedBuildHash == "" {
			continue
		}
		if consumers[service.ImageFrom] == nil {
			consumers[service.ImageFrom] = make(map[string]config.ServiceConfig)
		}
		consumers[service.ImageFrom][serviceName] = service
	}
	names := make([]string, 0, len(consumers))
	for name := range consumers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := cfg.Builds[name]; !ok {
			return fmt.Errorf("shared build %s is not defined", name)
		}
		if err := ensure(name, deployplan.SharedBuildImageRef(cfg, envName, name, buildTag), consumers[name]); err != nil {
			return err
		}
	}
	return nil
}

func buildSharedImagesWith(cfg *config.Config, envName string, buildTag string, services map[string]config.ServiceConfig, execute func(string, config.SharedBuildConfig, string, map[string]config.ServiceConfig) error) error {
	consumers := make(map[string]map[string]config.ServiceConfig)
	for serviceName, service := range services {
		if service.SharedBuildHash == "" {
			continue
		}
		if consumers[service.ImageFrom] == nil {
			consumers[service.ImageFrom] = make(map[string]config.ServiceConfig)
		}
		consumers[service.ImageFrom][serviceName] = service
	}
	names := make([]string, 0, len(consumers))
	for name := range consumers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		build, ok := cfg.Builds[name]
		if !ok {
			return fmt.Errorf("shared build %s is not defined", name)
		}
		imageRef := deployplan.SharedBuildImageRef(cfg, envName, name, buildTag)
		if err := execute(name, build, imageRef, consumers[name]); err != nil {
			return err
		}
	}
	return nil
}
