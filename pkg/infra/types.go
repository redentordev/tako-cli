package infra

import "time"

// ServerOutput contains provisioned server information
type ServerOutput struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	PublicIP  string `json:"public_ip"`
	PrivateIP string `json:"private_ip,omitempty"`
	Status    string `json:"status"`
	Region    string `json:"region"`
	Role      string `json:"role"`
	Index     int    `json:"index"` // For servers with count > 1
}

// NetworkOutput contains provisioned network information
type NetworkOutput struct {
	VPCID        string `json:"vpc_id,omitempty"`
	VPCName      string `json:"vpc_name,omitempty"`
	FirewallID   string `json:"firewall_id,omitempty"`
	FirewallName string `json:"firewall_name,omitempty"`
}

// InfraState tracks the complete infrastructure state
type InfraState struct {
	Provider        string                  `json:"provider"`
	Region          string                  `json:"region"`
	Environment     string                  `json:"environment"`
	LastProvisioned time.Time               `json:"last_provisioned"`
	Servers         map[string]ServerOutput `json:"servers"`
	Network         *NetworkOutput          `json:"network,omitempty"`
	StackName       string                  `json:"stack_name"`
	Outputs         map[string]interface{}  `json:"outputs"`
}

// ProvisionResult contains the result of a provision operation
type ProvisionResult struct {
	Success   bool                    `json:"success"`
	Servers   map[string]ServerOutput `json:"servers"`
	Network   *NetworkOutput          `json:"network,omitempty"`
	Outputs   map[string]interface{}  `json:"outputs"`
	Summary   string                  `json:"summary"`
	Error     string                  `json:"error,omitempty"`
	Duration  time.Duration           `json:"duration"`
}

// ProvisionOptions configures the provision operation
type ProvisionOptions struct {
	Preview     bool   // Preview changes without applying
	Environment string // Target environment
	Verbose     bool   // Verbose output
	Yes         bool   // Skip confirmation prompts
}

// DestroyOptions configures the destroy operation
type DestroyOptions struct {
	Preview     bool   // Preview what would be destroyed
	Environment string // Target environment
	Verbose     bool   // Verbose output
	Yes         bool   // Skip confirmation prompts
}
