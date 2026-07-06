package deployplan

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/reconcile"
)

// ServiceRevisionID returns the stable revision identity for a service deployment.
func ServiceRevisionID(project string, environment string, serviceName string, imageRef string, service config.ServiceConfig) string {
	configHash, ok := reconcile.SafeServiceConfigHash(service)
	if !ok {
		configHash = "unknown"
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{
		project,
		environment,
		serviceName,
		imageRef,
		configHash,
		EffectiveDeployStrategy(&service),
	}, "\x00")))
	return hex.EncodeToString(sum[:])[:12]
}

// EffectiveDeployStrategy returns the configured deploy strategy, defaulting to recreate.
func EffectiveDeployStrategy(service *config.ServiceConfig) string {
	if service == nil || service.Deploy.Strategy == "" {
		return config.DeployStrategyRecreate
	}
	return service.Deploy.Strategy
}
