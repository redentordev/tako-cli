package deployplan

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

const sharedBuildImageRefPrefix = "\x00build:"

func SharedBuildImageRefKey(name string) string { return sharedBuildImageRefPrefix + name }

func SharedBuildImageRef(cfg *config.Config, envName string, name string, buildTag string) string {
	build, ok := cfg.Builds[name]
	if !ok {
		return ""
	}
	baseTag := strings.TrimSpace(buildTag)
	if baseTag == "" {
		baseTag = fmt.Sprintf("%s-%s", cfg.Project.Version, envName)
	}
	return cfg.GetFullImageNameWithTag("shared/"+name, SharedBuildTag(baseTag, build.Fingerprint()))
}

func SharedBuildTag(baseTag string, fingerprint string) string {
	suffix := "-sb-" + fingerprint
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}
	maxBase := maxDockerTagLength - len(suffix)
	if len(baseTag) > maxBase {
		baseTag = baseTag[:maxBase]
	}
	return baseTag + suffix
}

// DefaultImageRefs returns the default runtime image reference for each service.
func DefaultImageRefs(cfg *config.Config, envName string, services map[string]config.ServiceConfig) map[string]string {
	imageRefs := make(map[string]string, len(services))
	addSharedBuildImageRefs(cfg, envName, "", imageRefs)
	for serviceName, service := range services {
		if service.Image != "" {
			imageRefs[serviceName] = service.Image
		} else if _, ok := cfg.Builds[service.ImageFrom]; ok {
			imageRefs[serviceName] = imageRefs[SharedBuildImageRefKey(service.ImageFrom)]
		} else {
			imageRefs[serviceName] = cfg.GetFullImageName(serviceName, envName)
		}
	}
	resolveRunImageFromRefs(services, imageRefs)
	return imageRefs
}

// DefaultDeployImageRefs returns image references to use for a deploy operation.
func DefaultDeployImageRefs(cfg *config.Config, envName string, services map[string]config.ServiceConfig, buildTag string) map[string]string {
	imageRefs := make(map[string]string, len(services))
	addSharedBuildImageRefs(cfg, envName, buildTag, imageRefs)
	for serviceName, service := range services {
		imageRefs[serviceName] = ImageRef(cfg, envName, serviceName, service, buildTag)
	}
	resolveRunImageFromRefs(services, imageRefs)
	return imageRefs
}

func resolveRunImageFromRefs(services map[string]config.ServiceConfig, imageRefs map[string]string) {
	for serviceName, service := range services {
		if service.IsRun() && service.ImageFrom != "" && service.SharedBuildHash == "" && imageRefs[service.ImageFrom] != "" {
			imageRefs[serviceName] = imageRefs[service.ImageFrom]
		}
	}
}

// ImageRef returns the image reference for a single service during deploy planning.
func ImageRef(cfg *config.Config, envName string, serviceName string, service config.ServiceConfig, buildTag string) string {
	if service.Image != "" {
		return service.Image
	}
	if _, ok := cfg.Builds[service.ImageFrom]; ok {
		return SharedBuildImageRef(cfg, envName, service.ImageFrom, buildTag)
	}
	if service.Build != "" && buildTag != "" {
		return cfg.GetFullImageNameWithTag(serviceName, buildTag)
	}
	return cfg.GetFullImageName(serviceName, envName)
}

func addSharedBuildImageRefs(cfg *config.Config, envName string, buildTag string, imageRefs map[string]string) {
	for name := range cfg.Builds {
		imageRefs[SharedBuildImageRefKey(name)] = SharedBuildImageRef(cfg, envName, name, buildTag)
	}
}

// MergeRuntimeImageRefs combines default, deployed, and actual runtime image references.
func MergeRuntimeImageRefs(
	cfg *config.Config,
	envName string,
	services map[string]config.ServiceConfig,
	deployedImageRefs map[string]string,
	actualState map[string]*reconcile.ActualService,
) map[string]string {
	imageRefs := DefaultImageRefs(cfg, envName, services)
	for serviceName := range services {
		if imageRef := deployedImageRefs[serviceName]; imageRef != "" {
			imageRefs[serviceName] = imageRef
			continue
		}
		if actual := actualState[serviceName]; actual != nil && actual.Image != "" {
			imageRefs[serviceName] = actual.Image
		}
	}
	resolveRunImageFromRefs(services, imageRefs)
	return imageRefs
}
