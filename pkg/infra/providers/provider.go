package providers

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
)

// Provider interface that all cloud providers must implement
type Provider interface {
	// Name returns the provider identifier (digitalocean, hetzner, aws, etc.)
	Name() string

	// Configure sets up the provider with credentials
	Configure(ctx *pulumi.Context, creds config.InfraCredentialsConfig) error

	// CreateSSHKey uploads/creates an SSH key and returns the key ID
	// publicKey: the SSH public key content
	// keyName: name for the key on the provider
	CreateSSHKey(ctx *pulumi.Context, publicKey string, keyName string) (pulumi.StringOutput, error)

	// CreateServer provisions a server and returns server IDs for firewall attachment
	// name: the config name (e.g., "web") used for state mapping
	// resourceName: the cloud resource name (e.g., "myproject-web") used to avoid naming conflicts
	// autoSSHKeyID: optional auto-generated SSH key ID to use if no SSH keys specified
	// autoSSHPublicKey: optional public key content (for providers like Linode that use content directly)
	CreateServer(ctx *pulumi.Context, name string, spec config.InfraServerSpec, region string, vpcID pulumi.StringInput, resourceName string, autoSSHKeyID pulumi.StringOutput, autoSSHPublicKey string) ([]pulumi.StringInput, error)

	// CreateVPC provisions a VPC/private network
	CreateVPC(ctx *pulumi.Context, name string, spec *config.InfraVPCConfig, region string) (pulumi.StringOutput, error)

	// CreateFirewall provisions firewall rules
	CreateFirewall(ctx *pulumi.Context, name string, spec *config.InfraFirewallConfig, serverIDs []pulumi.StringInput) error

	// ValidateConfig validates provider-specific configuration
	ValidateConfig(infra *config.InfrastructureConfig) error

	// GetDefaultImage returns the default OS image for this provider
	GetDefaultImage() string

	// GetDefaultSize returns the default server size for this provider
	GetDefaultSize() string
}

// Registry maps provider names to constructor functions
var Registry = map[string]func() Provider{
	"digitalocean": NewDigitalOcean,
	"hetzner":      NewHetzner,
	"aws":          NewAWS,
	"linode":       NewLinode,
}

// Get returns a provider by name
func Get(name string) (Provider, error) {
	constructor, ok := Registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown infrastructure provider: %s (supported: digitalocean, hetzner, aws, linode)", name)
	}
	return constructor(), nil
}

// GetSupportedProviders returns a list of supported provider names
func GetSupportedProviders() []string {
	providers := make([]string, 0, len(Registry))
	for name := range Registry {
		providers = append(providers, name)
	}
	return providers
}
