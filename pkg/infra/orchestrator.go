package infra

import (
	"context"
	"fmt"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/infra/providers"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Orchestrator manages infrastructure provisioning operations
type Orchestrator struct {
	config        *config.Config
	stateManager  *StateManager
	provider      providers.Provider
	sshKeyManager *SSHKeyManager
	verbose       bool
}

// NewOrchestrator creates a new infrastructure orchestrator
func NewOrchestrator(cfg *config.Config, takoDir string, environment string, verbose bool) (*Orchestrator, error) {
	if cfg.Infrastructure == nil {
		return nil, fmt.Errorf("no infrastructure section defined in config file")
	}

	// Get the provider
	provider, err := providers.Get(cfg.Infrastructure.Provider)
	if err != nil {
		return nil, err
	}

	// Validate provider-specific config
	if err := provider.ValidateConfig(cfg.Infrastructure); err != nil {
		return nil, fmt.Errorf("invalid infrastructure config: %w", err)
	}

	stateManager := NewStateManagerWithConfig(takoDir, cfg.Project.Name, environment, cfg.Infrastructure)
	sshKeyManager := NewSSHKeyManager(takoDir)

	return &Orchestrator{
		config:        cfg,
		stateManager:  stateManager,
		provider:      provider,
		sshKeyManager: sshKeyManager,
		verbose:       verbose,
	}, nil
}

// createPulumiProgram returns the Pulumi program function for provisioning
func (o *Orchestrator) createPulumiProgram(sshKeyPair *SSHKeyPair) pulumi.RunFunc {
	return func(ctx *pulumi.Context) error {
		infra := o.config.Infrastructure

		// Configure the provider
		if err := o.provider.Configure(ctx, infra.Credentials); err != nil {
			return fmt.Errorf("failed to configure provider: %w", err)
		}

		// Create SSH key on provider if auto-generated
		var sshKeyID pulumi.StringOutput
		var autoSSHPublicKey string
		if sshKeyPair != nil && sshKeyPair.PublicKey != "" {
			keyName := fmt.Sprintf("tako-%s", o.config.Project.Name)
			var err error
			sshKeyID, err = o.provider.CreateSSHKey(ctx, sshKeyPair.PublicKey, keyName)
			if err != nil {
				return fmt.Errorf("failed to create SSH key: %w", err)
			}
			autoSSHPublicKey = sshKeyPair.PublicKey
		}

		// Create VPC if enabled
		var vpcID pulumi.StringInput // Use StringInput (interface) to properly handle nil
		if infra.Networking != nil && infra.Networking.VPC != nil && infra.Networking.VPC.Enabled {
			var err error
			vpcID, err = o.provider.CreateVPC(ctx, o.config.Project.Name, infra.Networking.VPC, infra.Region)
			if err != nil {
				return fmt.Errorf("failed to create VPC: %w", err)
			}
		}

		// Create servers and collect IDs for firewall
		// Server names are prefixed with project name to avoid conflicts
		allServerIDs := []pulumi.StringInput{}
		for name, spec := range infra.Servers {
			// Use project-prefixed name for cloud resource to avoid naming conflicts
			resourceName := fmt.Sprintf("%s-%s", o.config.Project.Name, name)
			serverIDs, err := o.provider.CreateServer(ctx, name, spec, infra.Region, vpcID, resourceName, sshKeyID, autoSSHPublicKey)
			if err != nil {
				return fmt.Errorf("failed to create server %s: %w", name, err)
			}
			allServerIDs = append(allServerIDs, serverIDs...)
		}

		// Create firewall if enabled
		if infra.Networking != nil && infra.Networking.Firewall != nil && infra.Networking.Firewall.Enabled {
			if err := o.provider.CreateFirewall(ctx, o.config.Project.Name, infra.Networking.Firewall, allServerIDs); err != nil {
				return fmt.Errorf("failed to create firewall: %w", err)
			}
		}

		// Export metadata
		ctx.Export("provider", pulumi.String(infra.Provider))
		ctx.Export("region", pulumi.String(infra.Region))
		ctx.Export("project", pulumi.String(o.config.Project.Name))

		// Export auto SSH key info if used
		if sshKeyPair != nil {
			ctx.Export("auto_ssh_key", pulumi.Bool(true))
			ctx.Export("ssh_key_path", pulumi.String(sshKeyPair.PrivateKeyPath))
		}

		// Export provider SSH key ID for reference
		if sshKeyID != (pulumi.StringOutput{}) {
			ctx.Export("provider_ssh_key_id", sshKeyID)
		}

		return nil
	}
}

// needsAutoSSH checks if any server needs an auto-generated SSH key
func (o *Orchestrator) needsAutoSSH() bool {
	infra := o.config.Infrastructure
	if infra == nil {
		return false
	}

	// Check if any server has no SSH keys configured
	for _, spec := range infra.Servers {
		if len(spec.SSHKeys) == 0 {
			return true
		}
	}
	return false
}

// ensureSSHKey generates SSH key pair if needed
func (o *Orchestrator) ensureSSHKey() (*SSHKeyPair, error) {
	if !o.needsAutoSSH() {
		return nil, nil
	}

	keyPair, err := o.sshKeyManager.EnsureKeyPair(o.config.Project.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to generate SSH key: %w", err)
	}

	return keyPair, nil
}

// Preview shows what infrastructure changes would be made
func (o *Orchestrator) Preview(ctx context.Context) (*ProvisionResult, error) {
	startTime := time.Now()

	// Ensure SSH key if needed
	sshKeyPair, err := o.ensureSSHKey()
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	stack, err := o.stateManager.CreateStack(ctx, o.createPulumiProgram(sshKeyPair))
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	result, err := o.stateManager.Preview(ctx, stack, o.verbose)
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	return &ProvisionResult{
		Success:  true,
		Summary:  result.StdOut,
		Duration: time.Since(startTime),
	}, nil
}

// Provision creates or updates infrastructure
func (o *Orchestrator) Provision(ctx context.Context) (*ProvisionResult, error) {
	startTime := time.Now()

	// Ensure SSH key if needed
	sshKeyPair, err := o.ensureSSHKey()
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	if sshKeyPair != nil && o.verbose {
		fmt.Printf("Using auto-generated SSH key: %s\n", sshKeyPair.PrivateKeyPath)
	}

	stack, err := o.stateManager.CreateStack(ctx, o.createPulumiProgram(sshKeyPair))
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	result, err := o.stateManager.Up(ctx, stack, o.verbose)
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	// Save outputs for Tako to use
	if err := o.stateManager.SaveOutputs(result.Outputs); err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    fmt.Sprintf("provision succeeded but failed to save outputs: %v", err),
			Duration: time.Since(startTime),
		}, err
	}

	// Parse and save infrastructure state
	infraState := o.parseOutputsToState(result.Outputs)
	if err := o.stateManager.SaveInfraState(infraState); err != nil {
		// Non-fatal, outputs are already saved
		fmt.Printf("Warning: failed to save infrastructure state: %v\n", err)
	}

	// Convert outputs to serializable format
	outputs := make(map[string]interface{})
	for k, v := range result.Outputs {
		outputs[k] = v.Value
	}

	return &ProvisionResult{
		Success:  true,
		Servers:  infraState.Servers,
		Network:  infraState.Network,
		Outputs:  outputs,
		Summary:  result.StdOut,
		Duration: time.Since(startTime),
	}, nil
}

// Destroy tears down infrastructure
func (o *Orchestrator) Destroy(ctx context.Context) (*ProvisionResult, error) {
	startTime := time.Now()

	// Load existing infrastructure state to get server IPs for host key cleanup
	infraState, _ := o.stateManager.LoadInfraState()
	var serverIPs []string
	if infraState != nil {
		for _, server := range infraState.Servers {
			if server.PublicIP != "" {
				serverIPs = append(serverIPs, server.PublicIP)
			}
		}
	}

	// Load existing SSH key if one was used (needed for Pulumi to track resources)
	sshKeyPair := o.sshKeyManager.GetKeyPair()

	stack, err := o.stateManager.CreateStack(ctx, o.createPulumiProgram(sshKeyPair))
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	result, err := o.stateManager.Destroy(ctx, stack, o.verbose)
	if err != nil {
		return &ProvisionResult{
			Success:  false,
			Error:    err.Error(),
			Duration: time.Since(startTime),
		}, err
	}

	// Clear local state
	if err := o.stateManager.ClearState(); err != nil {
		fmt.Printf("Warning: failed to clear infrastructure state: %v\n", err)
	}

	// Clean up SSH keys
	if err := o.sshKeyManager.CleanupKeys(); err != nil {
		fmt.Printf("Warning: failed to clean up SSH keys: %v\n", err)
	}

	// Clean up host keys for destroyed servers
	for _, ip := range serverIPs {
		if err := ssh.RemoveHostKey(ip); err != nil {
			if o.verbose {
				fmt.Printf("Warning: failed to remove host key for %s: %v\n", ip, err)
			}
		}
	}

	return &ProvisionResult{
		Success:  true,
		Summary:  result.StdOut,
		Duration: time.Since(startTime),
	}, nil
}

// GetOutputs retrieves the current infrastructure outputs
func (o *Orchestrator) GetOutputs() (map[string]interface{}, error) {
	return o.stateManager.LoadOutputs()
}

// GetInfraState retrieves the current infrastructure state
func (o *Orchestrator) GetInfraState() (*InfraState, error) {
	return o.stateManager.LoadInfraState()
}

// IsProvisioned checks if infrastructure has been provisioned
func (o *Orchestrator) IsProvisioned() bool {
	return o.stateManager.IsProvisioned()
}

// GetSSHKeyPair returns the current SSH key pair, loading state if needed
func (o *Orchestrator) GetSSHKeyPair() *SSHKeyPair {
	return o.sshKeyManager.GetKeyPair()
}

// parseOutputsToState converts Pulumi outputs to InfraState
func (o *Orchestrator) parseOutputsToState(outputMap auto.OutputMap) *InfraState {
	// Convert OutputMap to map[string]interface{}
	outputs := make(map[string]interface{})
	for k, v := range outputMap {
		outputs[k] = v.Value
	}
	state := &InfraState{
		Provider:    o.config.Infrastructure.Provider,
		Region:      o.config.Infrastructure.Region,
		Environment: o.stateManager.environment,
		Servers:     make(map[string]ServerOutput),
		Outputs:     make(map[string]interface{}),
	}

	// Parse server outputs
	for name, spec := range o.config.Infrastructure.Servers {
		count := spec.Count
		if count < 1 {
			count = 1
		}

		for i := 0; i < count; i++ {
			serverName := name
			if count > 1 {
				serverName = fmt.Sprintf("%s-%d", name, i)
			}

			server := ServerOutput{
				Name:   serverName,
				Region: o.config.Infrastructure.Region,
				Role:   spec.Role,
				Index:  i,
			}

			// Extract from outputs
			if id, ok := outputs[serverName+"_id"]; ok {
				server.ID = fmt.Sprintf("%v", id)
			}
			if ip, ok := outputs[serverName+"_ip"]; ok {
				server.PublicIP = fmt.Sprintf("%v", ip)
			}
			if pip, ok := outputs[serverName+"_private_ip"]; ok {
				server.PrivateIP = fmt.Sprintf("%v", pip)
			}
			if status, ok := outputs[serverName+"_status"]; ok {
				server.Status = fmt.Sprintf("%v", status)
			}

			state.Servers[serverName] = server
		}
	}

	// Parse network outputs
	if o.config.Infrastructure.Networking != nil {
		state.Network = &NetworkOutput{}
		if vpcID, ok := outputs["vpc_id"]; ok {
			state.Network.VPCID = fmt.Sprintf("%v", vpcID)
		}
		if vpcName, ok := outputs["vpc_name"]; ok {
			state.Network.VPCName = fmt.Sprintf("%v", vpcName)
		}
		if fwID, ok := outputs["firewall_id"]; ok {
			state.Network.FirewallID = fmt.Sprintf("%v", fwID)
		}
		if fwName, ok := outputs["firewall_name"]; ok {
			state.Network.FirewallName = fmt.Sprintf("%v", fwName)
		}
	}

	// Copy all outputs
	for k, v := range outputs {
		state.Outputs[k] = v
	}

	return state
}
