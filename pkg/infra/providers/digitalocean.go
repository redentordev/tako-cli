package providers

import (
	"fmt"
	"strconv"

	"github.com/pulumi/pulumi-digitalocean/sdk/v4/go/digitalocean"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
)

// DigitalOceanProvider implements the Provider interface for DigitalOcean
type DigitalOceanProvider struct {
	token string
}

// NewDigitalOcean creates a new DigitalOcean provider
func NewDigitalOcean() Provider {
	return &DigitalOceanProvider{}
}

// Name returns the provider identifier
func (p *DigitalOceanProvider) Name() string {
	return "digitalocean"
}

// Configure sets up the provider with credentials
func (p *DigitalOceanProvider) Configure(ctx *pulumi.Context, creds config.InfraCredentialsConfig) error {
	if creds.Token == "" {
		return fmt.Errorf("DigitalOcean requires a token (set credentials.token or DIGITALOCEAN_TOKEN env var)")
	}
	p.token = creds.Token

	// Create provider instance with token
	_, err := digitalocean.NewProvider(ctx, "do-provider", &digitalocean.ProviderArgs{
		Token: pulumi.String(p.token),
	})
	if err != nil {
		return fmt.Errorf("failed to create DigitalOcean provider: %w", err)
	}

	return nil
}

// CreateSSHKey uploads an SSH key to DigitalOcean and returns the key ID
func (p *DigitalOceanProvider) CreateSSHKey(ctx *pulumi.Context, publicKey string, keyName string) (pulumi.StringOutput, error) {
	sshKey, err := digitalocean.NewSshKey(ctx, keyName, &digitalocean.SshKeyArgs{
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

// CreateServer provisions a DigitalOcean Droplet
func (p *DigitalOceanProvider) CreateServer(ctx *pulumi.Context, name string, spec config.InfraServerSpec, region string, vpcID pulumi.StringInput, resourceName string, autoSSHKeyID pulumi.StringOutput, autoSSHPublicKey string) ([]pulumi.StringInput, error) {
	// Default values
	size := spec.Size
	if size == "" {
		size = p.GetDefaultSize()
	}

	image := spec.Image
	if image == "" {
		image = p.GetDefaultImage()
	}

	count := spec.Count
	if count < 1 {
		count = 1
	}

	// Collect server IDs for firewall attachment
	serverIDs := make([]pulumi.StringInput, 0, count)

	// Create droplets
	for i := 0; i < count; i++ {
		// Use resourceName for the cloud resource (includes project prefix)
		dropletName := resourceName
		if count > 1 {
			dropletName = fmt.Sprintf("%s-%d", resourceName, i)
		}
		// configName is used for state/output keys
		configName := name
		if count > 1 {
			configName = fmt.Sprintf("%s-%d", name, i)
		}

		args := &digitalocean.DropletArgs{
			Name:   pulumi.String(dropletName),
			Size:   pulumi.String(size),
			Image:  pulumi.String(image),
			Region: pulumi.String(region),
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

		// Add tags if specified
		if len(spec.Tags) > 0 {
			tagArgs := pulumi.StringArray{}
			for _, tag := range spec.Tags {
				tagArgs = append(tagArgs, pulumi.String(tag))
			}
			args.Tags = tagArgs
		}

		// Add user data (cloud-init) if specified
		if spec.UserData != "" {
			args.UserData = pulumi.String(spec.UserData)
		}

		// Add VPC if provided
		if vpcID != nil {
			if strOutput, ok := vpcID.(pulumi.StringOutput); ok {
				args.VpcUuid = strOutput
			}
		}

		// Create the droplet
		droplet, err := digitalocean.NewDroplet(ctx, dropletName, args)
		if err != nil {
			return nil, fmt.Errorf("failed to create droplet %s: %w", dropletName, err)
		}

		// Collect server ID for firewall
		serverIDs = append(serverIDs, droplet.ID().ToStringOutput())

		// Export outputs using configName (for state mapping)
		ctx.Export(fmt.Sprintf("%s_id", configName), droplet.ID())
		ctx.Export(fmt.Sprintf("%s_ip", configName), droplet.Ipv4Address)
		ctx.Export(fmt.Sprintf("%s_private_ip", configName), droplet.Ipv4AddressPrivate)
		ctx.Export(fmt.Sprintf("%s_status", configName), droplet.Status)
		ctx.Export(fmt.Sprintf("%s_region", configName), droplet.Region)

		// Export role for Tako to use
		role := spec.Role
		if role == "" {
			role = "worker"
		}
		ctx.Export(fmt.Sprintf("%s_role", configName), pulumi.String(role))
	}

	return serverIDs, nil
}

// CreateVPC provisions a DigitalOcean VPC
func (p *DigitalOceanProvider) CreateVPC(ctx *pulumi.Context, name string, spec *config.InfraVPCConfig, region string) (pulumi.StringOutput, error) {
	if spec == nil || !spec.Enabled {
		return pulumi.StringOutput{}, nil
	}

	vpcName := spec.Name
	if vpcName == "" {
		vpcName = fmt.Sprintf("%s-vpc", name)
	}

	ipRange := spec.IPRange
	if ipRange == "" {
		ipRange = "10.0.0.0/16"
	}

	vpc, err := digitalocean.NewVpc(ctx, vpcName, &digitalocean.VpcArgs{
		Name:    pulumi.String(vpcName),
		Region:  pulumi.String(region),
		IpRange: pulumi.String(ipRange),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create VPC: %w", err)
	}

	ctx.Export("vpc_id", vpc.ID())
	ctx.Export("vpc_name", vpc.Name)
	ctx.Export("vpc_ip_range", vpc.IpRange)

	return vpc.ID().ToStringOutput(), nil
}

// CreateFirewall provisions a DigitalOcean Firewall
func (p *DigitalOceanProvider) CreateFirewall(ctx *pulumi.Context, name string, spec *config.InfraFirewallConfig, serverIDs []pulumi.StringInput) error {
	if spec == nil || !spec.Enabled {
		return nil
	}

	fwName := spec.Name
	if fwName == "" {
		fwName = fmt.Sprintf("%s-firewall", name)
	}

	// Convert rules to DigitalOcean format
	inboundRules := digitalocean.FirewallInboundRuleArray{}
	for _, rule := range spec.Rules {
		// Convert ports to port range string
		portRange := "all"
		if len(rule.Ports) > 0 {
			if len(rule.Ports) == 1 {
				portRange = fmt.Sprintf("%d", rule.Ports[0])
			} else {
				// For multiple ports, create separate rules
				for _, port := range rule.Ports {
					inboundRules = append(inboundRules, &digitalocean.FirewallInboundRuleArgs{
						Protocol:        pulumi.String(rule.Protocol),
						PortRange:       pulumi.String(fmt.Sprintf("%d", port)),
						SourceAddresses: pulumi.ToStringArray(rule.Sources),
					})
				}
				continue
			}
		}

		inboundRules = append(inboundRules, &digitalocean.FirewallInboundRuleArgs{
			Protocol:        pulumi.String(rule.Protocol),
			PortRange:       pulumi.String(portRange),
			SourceAddresses: pulumi.ToStringArray(rule.Sources),
		})
	}

	// Allow all outbound traffic by default
	outboundRules := digitalocean.FirewallOutboundRuleArray{
		&digitalocean.FirewallOutboundRuleArgs{
			Protocol:              pulumi.String("tcp"),
			PortRange:             pulumi.String("all"),
			DestinationAddresses:  pulumi.StringArray{pulumi.String("0.0.0.0/0"), pulumi.String("::/0")},
		},
		&digitalocean.FirewallOutboundRuleArgs{
			Protocol:              pulumi.String("udp"),
			PortRange:             pulumi.String("all"),
			DestinationAddresses:  pulumi.StringArray{pulumi.String("0.0.0.0/0"), pulumi.String("::/0")},
		},
		&digitalocean.FirewallOutboundRuleArgs{
			Protocol:              pulumi.String("icmp"),
			DestinationAddresses:  pulumi.StringArray{pulumi.String("0.0.0.0/0"), pulumi.String("::/0")},
		},
	}

	// Convert server IDs
	dropletIDs := pulumi.IntArray{}
	for _, id := range serverIDs {
		// Convert string ID to int (DigitalOcean uses int IDs)
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
				dropletIDs = append(dropletIDs, intOut)
			}
		}
	}

	fw, err := digitalocean.NewFirewall(ctx, fwName, &digitalocean.FirewallArgs{
		Name:          pulumi.String(fwName),
		DropletIds:    dropletIDs,
		InboundRules:  inboundRules,
		OutboundRules: outboundRules,
	})
	if err != nil {
		return fmt.Errorf("failed to create firewall: %w", err)
	}

	ctx.Export("firewall_id", fw.ID())
	ctx.Export("firewall_name", fw.Name)

	return nil
}

// ValidateConfig validates DigitalOcean-specific configuration
func (p *DigitalOceanProvider) ValidateConfig(infra *config.InfrastructureConfig) error {
	if infra.Credentials.Token == "" {
		return fmt.Errorf("DigitalOcean requires credentials.token")
	}

	if infra.Region == "" {
		return fmt.Errorf("DigitalOcean requires a region (e.g., nyc1, sfo3, ams3)")
	}

	// Validate server specs
	for name, spec := range infra.Servers {
		if spec.Size == "" && spec.Image == "" {
			// Will use defaults, that's fine
			continue
		}
		_ = name // Validation passed
	}

	return nil
}

// GetDefaultImage returns the default OS image for DigitalOcean
func (p *DigitalOceanProvider) GetDefaultImage() string {
	return "ubuntu-22-04-x64"
}

// GetDefaultSize returns the default server size for DigitalOcean
func (p *DigitalOceanProvider) GetDefaultSize() string {
	return "s-1vcpu-1gb"
}
