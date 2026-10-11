package config

import (
	"fmt"
	"strings"
	"time"
)

// Self-healing defaults and bounds (see docs/SELF-HEALING.md).
const (
	DefaultSelfHealingUnhealthyThreshold = 3
	DefaultSelfHealingMaxRestarts        = 3
	DefaultSelfHealingWindow             = 30 * time.Minute

	MinSelfHealingThreshold = 1
	MaxSelfHealingThreshold = 20
	MinSelfHealingWindow    = 5 * time.Minute
	MaxSelfHealingWindow    = 24 * time.Hour
)

// SelfHealingConfig is the opt-in node-local self-healing policy. It may be
// set on an environment (defaults for its services) and on a service (per-key
// override). Every key is optional; unset keys fall back to the environment
// and then to the defaults. The block is never part of container identity.
type SelfHealingConfig struct {
	Enabled            *bool  `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	RestartUnhealthy   *bool  `yaml:"restartUnhealthy,omitempty" json:"restartUnhealthy,omitempty"`
	StartExited        *bool  `yaml:"startExited,omitempty" json:"startExited,omitempty"`
	UnhealthyThreshold int    `yaml:"unhealthyThreshold,omitempty" json:"unhealthyThreshold,omitempty"`
	MaxRestarts        int    `yaml:"maxRestarts,omitempty" json:"maxRestarts,omitempty"`
	Window             string `yaml:"window,omitempty" json:"window,omitempty"`
}

// SelfHealingPolicy is the resolved effective policy for one service.
type SelfHealingPolicy struct {
	Enabled            bool
	RestartUnhealthy   bool
	StartExited        bool
	UnhealthyThreshold int
	MaxRestarts        int
	Window             time.Duration
}

// EffectiveSelfHealing resolves a service's policy: service keys, else
// environment keys, else defaults. For a persistent service the
// environment-level enabled flag is ignored; only the service's own
// enabled: true turns healing on. Jobs and runs are never healed.
func EffectiveSelfHealing(env *EnvironmentConfig, service ServiceConfig) SelfHealingPolicy {
	policy := SelfHealingPolicy{
		RestartUnhealthy:   true,
		StartExited:        true,
		UnhealthyThreshold: DefaultSelfHealingUnhealthyThreshold,
		MaxRestarts:        DefaultSelfHealingMaxRestarts,
		Window:             DefaultSelfHealingWindow,
	}
	var envBlock *SelfHealingConfig
	if env != nil {
		envBlock = env.SelfHealing
	}
	apply := func(block *SelfHealingConfig, allowEnabled bool) {
		if block == nil {
			return
		}
		if allowEnabled && block.Enabled != nil {
			policy.Enabled = *block.Enabled
		}
		if block.RestartUnhealthy != nil {
			policy.RestartUnhealthy = *block.RestartUnhealthy
		}
		if block.StartExited != nil {
			policy.StartExited = *block.StartExited
		}
		if block.UnhealthyThreshold > 0 {
			policy.UnhealthyThreshold = block.UnhealthyThreshold
		}
		if block.MaxRestarts > 0 {
			policy.MaxRestarts = block.MaxRestarts
		}
		if window, err := time.ParseDuration(strings.TrimSpace(block.Window)); err == nil && window > 0 {
			policy.Window = window
		}
	}
	apply(envBlock, !service.Persistent)
	apply(service.SelfHealing, true)
	if service.IsJob() || service.IsRun() {
		policy.Enabled = false
	}
	if !policy.RestartUnhealthy && !policy.StartExited {
		policy.Enabled = false
	}
	return policy
}

// SelfHealingEnabled reports whether any service in the environment has an
// enabled effective self-healing policy.
func (c *Config) SelfHealingEnabled(environment string) bool {
	if c == nil {
		return false
	}
	env, ok := c.Environments[environment]
	if !ok {
		return false
	}
	for _, service := range env.Services {
		if EffectiveSelfHealing(&env, service).Enabled {
			return true
		}
	}
	return false
}

func validateSelfHealingConfig(path string, block *SelfHealingConfig) error {
	if block == nil {
		return nil
	}
	if block.UnhealthyThreshold != 0 && (block.UnhealthyThreshold < MinSelfHealingThreshold || block.UnhealthyThreshold > MaxSelfHealingThreshold) {
		return fmt.Errorf("%s: selfHealing.unhealthyThreshold must be between %d and %d", path, MinSelfHealingThreshold, MaxSelfHealingThreshold)
	}
	if block.MaxRestarts != 0 && (block.MaxRestarts < MinSelfHealingThreshold || block.MaxRestarts > MaxSelfHealingThreshold) {
		return fmt.Errorf("%s: selfHealing.maxRestarts must be between %d and %d", path, MinSelfHealingThreshold, MaxSelfHealingThreshold)
	}
	if value := strings.TrimSpace(block.Window); value != "" {
		window, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("%s: selfHealing.window must be a duration such as 30m: %v", path, err)
		}
		if window < MinSelfHealingWindow || window > MaxSelfHealingWindow {
			return fmt.Errorf("%s: selfHealing.window must be between 5m and 24h", path)
		}
	} else if block.Window != "" {
		return fmt.Errorf("%s: selfHealing.window must not be blank", path)
	}
	return nil
}
