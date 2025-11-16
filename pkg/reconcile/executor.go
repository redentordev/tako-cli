package reconcile

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Executor handles the execution of a reconciliation plan
type Executor struct {
	deployer *deployer.Deployer
	client   *ssh.Client
	config   *config.Config
	env      string
	verbose  bool
}

// NewExecutor creates a new plan executor
func NewExecutor(
	deployer *deployer.Deployer,
	client *ssh.Client,
	config *config.Config,
	env string,
	verbose bool,
) *Executor {
	return &Executor{
		deployer: deployer,
		client:   client,
		config:   config,
		env:      env,
		verbose:  verbose,
	}
}

// ExecutePlan executes a reconciliation plan
func (e *Executor) ExecutePlan(plan *ReconciliationPlan) error {
	if plan.IsEmpty() {
		if e.verbose {
			fmt.Println("✓ No changes needed")
		}
		return nil
	}

	// Track failures but continue with other services
	var failures []string

	// Execute changes in order: removes, then updates, then adds
	// This ensures we free up resources before consuming them

	// 1. Remove services no longer in config
	for _, change := range plan.Changes {
		if change.Type == ChangeRemove {
			if err := e.executeRemove(change); err != nil {
				errMsg := fmt.Sprintf("remove %s: %v", change.ServiceName, err)
				failures = append(failures, errMsg)
				if e.verbose {
					fmt.Printf("  ✗ Failed to remove service %s: %v\n", change.ServiceName, err)
				}
				// Continue with other removals
			}
		}
	}

	// 2. Update existing services
	for _, change := range plan.Changes {
		if change.Type == ChangeUpdate {
			if err := e.executeUpdate(change); err != nil {
				errMsg := fmt.Sprintf("update %s: %v", change.ServiceName, err)
				failures = append(failures, errMsg)
				if e.verbose {
					fmt.Printf("  ✗ Failed to update service %s: %v\n", change.ServiceName, err)
				}
				// Continue with other updates
			}
		}
	}

	// 3. Add new services
	for _, change := range plan.Changes {
		if change.Type == ChangeAdd {
			if err := e.executeAdd(change); err != nil {
				errMsg := fmt.Sprintf("add %s: %v", change.ServiceName, err)
				failures = append(failures, errMsg)
				if e.verbose {
					fmt.Printf("  ✗ Failed to add service %s: %v\n", change.ServiceName, err)
				}
				// Continue with other additions
			}
		}
	}

	// Report failures
	if len(failures) > 0 {
		return fmt.Errorf("deployment completed with %d errors:\n  - %s",
			len(failures), strings.Join(failures, "\n  - "))
	}

	return nil
}

// executeRemove removes a service
func (e *Executor) executeRemove(change ServiceChange) error {
	if e.verbose {
		fmt.Printf("\n→ Removing service: %s\n", change.ServiceName)
		for _, reason := range change.Reasons {
			fmt.Printf("  %s\n", reason)
		}
	}

	// Check if Swarm mode
	swarmCheck, _ := e.client.Execute("docker info --format '{{.Swarm.LocalNodeState}}'")
	isSwarm := strings.TrimSpace(swarmCheck) == "active"

	if isSwarm {
		// Remove Swarm service
		fullServiceName := fmt.Sprintf("%s_%s_%s", e.config.Project.Name, e.env, change.ServiceName)
		cmd := fmt.Sprintf("docker service rm %s", fullServiceName)
		if _, err := e.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to remove swarm service: %w", err)
		}
		if e.verbose {
			fmt.Printf("  ✓ Removed swarm service: %s\n", fullServiceName)
		}
	} else {
		// Stop and remove containers (standalone mode)
		// Container naming: {project}_{env}_{service}_{replica}
		prefix := fmt.Sprintf("%s_%s_%s", e.config.Project.Name, e.env, change.ServiceName)

		// Get container IDs matching this service
		listCmd := fmt.Sprintf("docker ps -aq -f name=%s", prefix)
		containerIDs, err := e.client.Execute(listCmd)
		containerIDs = strings.TrimSpace(containerIDs)

		if err == nil && containerIDs != "" {
			// Stop containers
			stopCmd := fmt.Sprintf("docker stop %s", containerIDs)
			if _, err := e.client.Execute(stopCmd); err != nil {
				if e.verbose {
					fmt.Printf("  ⚠ Failed to stop containers: %v\n", err)
				}
			}

			// Remove containers
			rmCmd := fmt.Sprintf("docker rm %s", containerIDs)
			if _, err := e.client.Execute(rmCmd); err != nil {
				return fmt.Errorf("failed to remove containers: %w", err)
			}

			if e.verbose {
				fmt.Printf("  ✓ Removed containers for service: %s\n", change.ServiceName)
			}
		} else {
			if e.verbose {
				fmt.Printf("  ⚠ No containers found for service: %s\n", change.ServiceName)
			}
		}
	}

	return nil
}

// executeUpdate updates a service (essentially redeploy)
func (e *Executor) executeUpdate(change ServiceChange) error {
	if e.verbose {
		fmt.Printf("\n→ Updating service: %s\n", change.ServiceName)
		for _, reason := range change.Reasons {
			fmt.Printf("  %s\n", reason)
		}
	}

	// Deploy service with new config
	// This will replace the old version
	return e.deployer.DeployService(change.ServiceName, change.NewConfig, false)
}

// executeAdd adds a new service
func (e *Executor) executeAdd(change ServiceChange) error {
	if e.verbose {
		fmt.Printf("\n→ Adding service: %s\n", change.ServiceName)
		for _, reason := range change.Reasons {
			fmt.Printf("  %s\n", reason)
		}
	}

	// Deploy new service
	return e.deployer.DeployService(change.ServiceName, change.NewConfig, false)
}
