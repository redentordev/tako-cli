package providers

import (
	"fmt"
	"strconv"

	"github.com/pulumi/pulumi-hcloud/sdk/go/hcloud"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
)

// HetznerProvider implements the Provider interface for Hetzner Cloud
type HetznerProvider struct {
	token string
}

// NewHetzner creates a new Hetzner Cloud provider
func NewHetzner() Provider {
	return &HetznerProvider{}
}

// Name returns the provider identifier
func (p *HetznerProvider) Name() string {
	return "hetzner"
}

// Configure sets up the provider with credentials
func (p *HetznerProvider) Configure(ctx *pulumi.Context, creds config.InfraCredentialsConfig) error {
	if creds.Token == "" {
		return fmt.Errorf("Hetzner requires a token (set credentials.token or HCLOUD_TOKEN env var)")
	}
	p.token = creds.Token

	// Create provider instance with token
	_, err := hcloud.NewProvider(ctx, "hcloud-provider", &hcloud.ProviderArgs{
		Token: pulumi.String(p.token),
	})
	if err != nil {
		return fmt.Errorf("failed to create Hetzner provider: %w", err)
	}

	return nil
}

// CreateSSHKey uploads an SSH key to Hetzner and returns the key ID
func (p *HetznerProvider) CreateSSHKey(ctx *pulumi.Context, publicKey string, keyName string) (pulumi.StringOutput, error) {
	sshKey, err := hcloud.NewSshKey(ctx, keyName, &hcloud.SshKeyArgs{
		Name:      pulumi.String(keyName),
		PublicKey: pulumi.String(publicKey),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create SSH key: %w", err)
	}

	ctx.Export("ssh_key_id", sshKey.ID())
	ctx.Export("ssh_key_name", sshKey.Name)
	ctx.Export("ssh_key_fingerprint", sshKey.Fingerprint)

	return sshKey.ID().ToStringOutput(), nil
}

// CreateServer provisions a Hetzner Cloud Server
func (p *HetznerProvider) CreateServer(ctx *pulumi.Context, name string, spec config.InfraServerSpec, region string, vpcID pulumi.StringInput, resourceName string, autoSSHKeyID pulumi.StringOutput, autoSSHPublicKey string) ([]pulumi.StringInput, error) {
	// Default values
	serverType := spec.Size
	if serverType == "" {
		serverType = p.GetDefaultSize()
	}

	image := spec.Image
	if image == "" {
		image = p.GetDefaultImage()
	}

	count := spec.Count
	if count < 1 {
		count = 1
	}

	// Convert tags to labels (Hetzner uses labels instead of tags)
	labels := make(map[string]string)
	for _, tag := range spec.Tags {
		labels[tag] = "true"
	}

	// Add role as a label
	role := spec.Role
	if role == "" {
		role = "worker"
	}
	labels["role"] = role

	// Collect server IDs for firewall attachment
	serverIDs := make([]pulumi.StringInput, 0, count)

	// Create servers
	for i := 0; i < count; i++ {
		// Use resourceName for the cloud resource (includes project prefix)
		serverName := resourceName
		if count > 1 {
			serverName = fmt.Sprintf("%s-%d", resourceName, i)
		}
		// configName is used for state/output keys
		configName := name
		if count > 1 {
			configName = fmt.Sprintf("%s-%d", name, i)
		}

		args := &hcloud.ServerArgs{
			Name:       pulumi.String(serverName),
			ServerType: pulumi.String(serverType),
			Image:      pulumi.String(image),
			Location:   pulumi.String(region),
			Labels:     pulumi.ToStringMap(labels),
		}

		// Add SSH keys if specified, or use auto-generated key
		if len(spec.SSHKeys) > 0 {
			sshKeyArgs := pulumi.StringArray{}
			for _, key := range spec.SSHKeys {
				sshKeyArgs = append(sshKeyArgs, pulumi.String(key))
			}
			args.SshKeys = sshKeyArgs
		} else if autoSSHKeyID != (pulumi.StringOutput{}) {
			// Use auto-generated SSH key
			args.SshKeys = pulumi.StringArray{autoSSHKeyID}
		}

		// Add user data (cloud-init) if specified
		if spec.UserData != "" {
			args.UserData = pulumi.String(spec.UserData)
		}

		// Add to network if VPC provided
		if vpcID != nil {
			if strOutput, ok := vpcID.(pulumi.StringOutput); ok {
				intOutput := strOutput.ApplyT(func(v interface{}) int {
					s, ok := v.(string)
					if !ok {
						return 0
					}
					i, err := strconv.Atoi(s)
					if err != nil {
						return 0 // Return 0 on parse error
					}
					return i
				})
				if intOut, ok := intOutput.(pulumi.IntOutput); ok {
					args.Networks = hcloud.ServerNetworkTypeArray{
						&hcloud.ServerNetworkTypeArgs{
							NetworkId: intOut,
						},
					}
				}
			}
		}

		// Create the server (use serverName for cloud resource)
		server, err := hcloud.NewServer(ctx, serverName, args)
		if err != nil {
			return nil, fmt.Errorf("failed to create server %s: %w", serverName, err)
		}

		// Collect server ID for firewall
		serverIDs = append(serverIDs, server.ID().ToStringOutput())

		// Export outputs using configName (for state mapping)
		ctx.Export(fmt.Sprintf("%s_id", configName), server.ID())
		ctx.Export(fmt.Sprintf("%s_ip", configName), server.Ipv4Address)
		// Hetzner private IPs are assigned when attached to a network
		// Export the first network IP if available, otherwise use the public IP
		ctx.Export(fmt.Sprintf("%s_private_ip", configName), server.Ipv4Address)
		ctx.Export(fmt.Sprintf("%s_status", configName), server.Status)
		ctx.Export(fmt.Sprintf("%s_region", configName), pulumi.String(region))
		ctx.Export(fmt.Sprintf("%s_role", configName), pulumi.String(role))
	}

	return serverIDs, nil
}

// CreateVPC provisions a Hetzner Cloud Network
func (p *HetznerProvider) CreateVPC(ctx *pulumi.Context, name string, spec *config.InfraVPCConfig, region string) (pulumi.StringOutput, error) {
	if spec == nil || !spec.Enabled {
		return pulumi.StringOutput{}, nil
	}

	networkName := spec.Name
	if networkName == "" {
		networkName = fmt.Sprintf("%s-network", name)
	}

	ipRange := spec.IPRange
	if ipRange == "" {
		ipRange = "10.0.0.0/16"
	}

	network, err := hcloud.NewNetwork(ctx, networkName, &hcloud.NetworkArgs{
		Name:    pulumi.String(networkName),
		IpRange: pulumi.String(ipRange),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create network: %w", err)
	}

	// Create a subnet for the network
	intOutput := network.ID().ApplyT(func(v interface{}) int {
		id, ok := v.(string)
		if !ok {
			return 0
		}
		i, err := strconv.Atoi(id)
		if err != nil {
			return 0 // Return 0 on parse error
		}
		return i
	})
	networkID, ok := intOutput.(pulumi.IntOutput)
	if !ok {
		return pulumi.StringOutput{}, fmt.Errorf("failed to convert network ID to int")
	}

	// Determine network zone from region
	networkZone := p.getNetworkZone(region)

	_, err = hcloud.NewNetworkSubnet(ctx, fmt.Sprintf("%s-subnet", networkName), &hcloud.NetworkSubnetArgs{
		NetworkId:   networkID,
		Type:        pulumi.String("cloud"),
		NetworkZone: pulumi.String(networkZone),
		IpRange:     pulumi.String(ipRange),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create subnet: %w", err)
	}

	ctx.Export("vpc_id", network.ID())
	ctx.Export("vpc_name", network.Name)
	ctx.Export("vpc_ip_range", network.IpRange)

	return network.ID().ToStringOutput(), nil
}

// CreateFirewall provisions a Hetzner Cloud Firewall
func (p *HetznerProvider) CreateFirewall(ctx *pulumi.Context, name string, spec *config.InfraFirewallConfig, serverIDs []pulumi.StringInput) error {
	if spec == nil || !spec.Enabled {
		return nil
	}

	fwName := spec.Name
	if fwName == "" {
		fwName = fmt.Sprintf("%s-firewall", name)
	}

	// Convert rules to Hetzner format
	rules := hcloud.FirewallRuleArray{}
	for _, rule := range spec.Rules {
		// Handle ports
		if len(rule.Ports) == 0 {
			// All ports
			rules = append(rules, &hcloud.FirewallRuleArgs{
				Direction:  pulumi.String("in"),
				Protocol:   pulumi.String(rule.Protocol),
				SourceIps:  pulumi.ToStringArray(rule.Sources),
			})
		} else {
			// Specific ports
			for _, port := range rule.Ports {
				rules = append(rules, &hcloud.FirewallRuleArgs{
					Direction:  pulumi.String("in"),
					Protocol:   pulumi.String(rule.Protocol),
					Port:       pulumi.String(fmt.Sprintf("%d", port)),
					SourceIps:  pulumi.ToStringArray(rule.Sources),
				})
			}
		}
	}

	// Convert server IDs for apply_to
	applyTo := hcloud.FirewallApplyToArray{}
	for _, id := range serverIDs {
		if strOutput, ok := id.(pulumi.StringOutput); ok {
			intOutput := strOutput.ApplyT(func(v interface{}) int {
				s, ok := v.(string)
				if !ok {
					return 0
				}
				i, err := strconv.Atoi(s)
				if err != nil {
					return 0 // Return 0 on parse error - firewall won't attach to invalid ID
				}
				return i
			})
			if intOut, ok := intOutput.(pulumi.IntOutput); ok {
				applyTo = append(applyTo, &hcloud.FirewallApplyToArgs{
					Server: intOut,
				})
			}
		}
	}

	fw, err := hcloud.NewFirewall(ctx, fwName, &hcloud.FirewallArgs{
		Name:     pulumi.String(fwName),
		Rules:    rules,
		ApplyTos: applyTo,
	})
	if err != nil {
		return fmt.Errorf("failed to create firewall: %w", err)
	}

	ctx.Export("firewall_id", fw.ID())
	ctx.Export("firewall_name", fw.Name)

	return nil
}

// ValidateConfig validates Hetzner-specific configuration
func (p *HetznerProvider) ValidateConfig(infra *config.InfrastructureConfig) error {
	if infra.Credentials.Token == "" {
		return fmt.Errorf("Hetzner requires credentials.token")
	}

	if infra.Region == "" {
		return fmt.Errorf("Hetzner requires a region/location (e.g., nbg1, fsn1, hel1)")
	}

	return nil
}

// GetDefaultImage returns the default OS image for Hetzner
func (p *HetznerProvider) GetDefaultImage() string {
	return "ubuntu-22.04"
}

// GetDefaultSize returns the default server size for Hetzner
func (p *HetznerProvider) GetDefaultSize() string {
	return "cax11" // Smallest ARM-based Hetzner instance (2 vCPU, 4GB RAM)
}

// getNetworkZone returns the network zone for a given region
func (p *HetznerProvider) getNetworkZone(region string) string {
	// Hetzner network zones based on datacenter location
	// https://docs.hetzner.com/cloud/general/locations/
	switch region {
	case "ash":
		return "us-east"
	case "hil":
		return "us-west"
	case "sin":
		return "ap-southeast"
	case "fsn1", "nbg1", "hel1":
		return "eu-central"
	default:
		// Default to eu-central for unknown regions
		return "eu-central"
	}
}
