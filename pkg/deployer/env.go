package deployer

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// EnvManager handles environment variable management
type EnvManager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// NewEnvManager creates a new environment manager
func NewEnvManager(client *ssh.Client, projectName, environment string, verbose bool) *EnvManager {
	return &EnvManager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// LoadAndMerge loads environment variables from envFile and merges with explicit env vars
// Priority: explicit env > envFile
func (em *EnvManager) LoadAndMerge(service *config.ServiceConfig) map[string]string {
	var envFileVars map[string]string

	// Load from envFile if specified
	if service.EnvFile != "" {
		var err error
		envFileVars, err = config.LoadEnvFile(service.EnvFile)
		if err != nil {
			if em.verbose {
				fmt.Printf("  Warning: Could not load envFile %s: %v\n", service.EnvFile, err)
			}
			envFileVars = make(map[string]string)
		} else if em.verbose {
			fmt.Printf("  Loaded %d variables from %s\n", len(envFileVars), service.EnvFile)
		}
	} else {
		envFileVars = make(map[string]string)
	}

	// Merge with explicit env vars (explicit takes priority)
	return config.MergeEnvVars(service.Env, envFileVars)
}
