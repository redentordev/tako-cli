package providers

import (
	"fmt"
	"strconv"

	"github.com/pulumi/pulumi-linode/sdk/v4/go/linode"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
)

// LinodeProvider implements the Provider interface for Linode
type LinodeProvider struct {
	token string
}

// NewLinode creates a new Linode provider
func NewLinode() Provider {
	return &LinodeProvider{}
}

// Name returns the provider identifier
func (p *LinodeProvider) Name() string {
	return "linode"
}

// Configure sets up the provider with credentials
func (p *LinodeProvider) Configure(ctx *pulumi.Context, creds config.InfraCredentialsConfig) error {
	if creds.Token == "" {
		return fmt.Errorf("Linode requires a token (set credentials.token or LINODE_TOKEN env var)")
	}
	p.token = creds.Token

	// Create Linode provider instance
	_, err := linode.NewProvider(ctx, "linode-provider", &linode.ProviderArgs{
		Token: pulumi.String(p.token),
	})
	if err != nil {
		return fmt.Errorf("failed to create Linode provider: %w", err)
	}

	return nil
}

// CreateSSHKey uploads an SSH key to Linode and returns the key ID
func (p *LinodeProvider) CreateSSHKey(ctx *pulumi.Context, publicKey string, keyName string) (pulumi.StringOutput, error) {
	sshKey, err := linode.NewSshKey(ctx, keyName, &linode.SshKeyArgs{
		Label:     pulumi.String(keyName),
		SshKey:    pulumi.String(publicKey),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create SSH key: %w", err)
	}

	ctx.Export("ssh_key_id", sshKey.ID())
	ctx.Export("ssh_key_name", sshKey.Label) // Use ssh_key_name for consistency with other providers
	ctx.Export("ssh_key_label", sshKey.Label)

	return sshKey.ID().ToStringOutput(), nil
}

// CreateServer provisions a Linode instance
func (p *LinodeProvider) CreateServer(ctx *pulumi.Context, name string, spec config.InfraServerSpec, region string, vpcID pulumi.StringInput, resourceName string, autoSSHKeyID pulumi.StringOutput, autoSSHPublicKey string) ([]pulumi.StringInput, error) {
	// Default values
	instanceType := spec.Size
	if instanceType == "" {
		instanceType = p.GetDefaultSize()
	}

	// Map generic sizes to Linode types
	instanceType = p.mapSize(instanceType)

	image := spec.Image
	if image == "" {
		image = p.GetDefaultImage()
	}

	count := spec.Count
	if count < 1 {
		count = 1
	}

	// Convert tags
	tags := make([]string, 0, len(spec.Tags)+1)
	tags = append(tags, spec.Tags...)

	// Add role as a tag
	role := spec.Role
	if role == "" {
		role = "worker"
	}
	tags = append(tags, fmt.Sprintf("role:%s", role))

	// Collect server IDs for firewall attachment
	serverIDs := make([]pulumi.StringInput, 0, count)

	// Create instances
	for i := 0; i < count; i++ {
		// Use resourceName for the cloud resource (includes project prefix)
		instanceName := resourceName
		if count > 1 {
			instanceName = fmt.Sprintf("%s-%d", resourceName, i)
		}
		// configName is used for state/output keys
		configName := name
		if count > 1 {
			configName = fmt.Sprintf("%s-%d", name, i)
		}

		args := &linode.InstanceArgs{
			Label:  pulumi.String(instanceName),
			Type:   pulumi.String(instanceType),
			Image:  pulumi.String(image),
			Region: pulumi.String(region),
			Tags:   pulumi.ToStringArray(tags),
		}

		// Add SSH keys if specified (authorized_keys), or use auto-generated key
		if len(spec.SSHKeys) > 0 {
			args.AuthorizedKeys = pulumi.ToStringArray(spec.SSHKeys)
		} else if autoSSHPublicKey != "" {
			// Linode uses the actual public key content, not an ID
			args.AuthorizedKeys = pulumi.ToStringArray([]string{autoSSHPublicKey})
		}

		// Add user data (metadata) if specified
		// Linode supports cloud-init via the metadata service
		// Note: UserData should be base64-encoded
		if spec.UserData != "" {
			args.Metadatas = linode.InstanceMetadataArray{
				&linode.InstanceMetadataArgs{
					UserData: pulumi.String(spec.UserData),
				},
			}
		}

		// Create the instance
		instance, err := linode.NewInstance(ctx, instanceName, args)
		if err != nil {
			return nil, fmt.Errorf("failed to create instance %s: %w", instanceName, err)
		}

		// Collect server ID for firewall
		serverIDs = append(serverIDs, instance.ID().ToStringOutput())

		// Export outputs
		ctx.Export(fmt.Sprintf("%s_id", configName), instance.ID())
		ctx.Export(fmt.Sprintf("%s_ip", configName), instance.IpAddress)
		ctx.Export(fmt.Sprintf("%s_private_ip", configName), instance.PrivateIpAddress)
		ctx.Export(fmt.Sprintf("%s_status", configName), instance.Status)
		ctx.Export(fmt.Sprintf("%s_region", configName), instance.Region)
		ctx.Export(fmt.Sprintf("%s_role", configName), pulumi.String(role))
	}

	return serverIDs, nil
}

// CreateVPC provisions a Linode VPC
func (p *LinodeProvider) CreateVPC(ctx *pulumi.Context, name string, spec *config.InfraVPCConfig, region string) (pulumi.StringOutput, error) {
	if spec == nil || !spec.Enabled {
		return pulumi.StringOutput{}, nil
	}

	vpcName := spec.Name
	if vpcName == "" {
		vpcName = fmt.Sprintf("%s-vpc", name)
	}

	// Create the VPC
	vpc, err := linode.NewVpc(ctx, vpcName, &linode.VpcArgs{
		Label:       pulumi.String(vpcName),
		Region:      pulumi.String(region),
		Description: pulumi.String(fmt.Sprintf("VPC for %s", name)),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create VPC: %w", err)
	}

	// Create a subnet
	ipRange := spec.IPRange
	if ipRange == "" {
		ipRange = "10.0.0.0/16"
	}

	// Convert VPC ID from string to int
	vpcIDOutput := vpc.ID().ApplyT(func(v interface{}) int {
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
	vpcIDInt, ok := vpcIDOutput.(pulumi.IntOutput)
	if !ok {
		return pulumi.StringOutput{}, fmt.Errorf("failed to convert VPC ID to int")
	}

	subnet, err := linode.NewVpcSubnet(ctx, fmt.Sprintf("%s-subnet", vpcName), &linode.VpcSubnetArgs{
		VpcId: vpcIDInt,
		Label: pulumi.String(fmt.Sprintf("%s-subnet", vpcName)),
		Ipv4:  pulumi.String(ipRange),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create subnet: %w", err)
	}

	ctx.Export("vpc_id", vpc.ID())
	ctx.Export("vpc_name", vpc.Label)
	ctx.Export("subnet_id", subnet.ID())

	return vpc.ID().ToStringOutput(), nil
}

// CreateFirewall provisions a Linode Firewall
func (p *LinodeProvider) CreateFirewall(ctx *pulumi.Context, name string, spec *config.InfraFirewallConfig, serverIDs []pulumi.StringInput) error {
	if spec == nil || !spec.Enabled {
		return nil
	}

	fwName := spec.Name
	if fwName == "" {
		fwName = fmt.Sprintf("%s-firewall", name)
	}

	// Build inbound rules
	inboundRules := linode.FirewallInboundArray{}
	for _, rule := range spec.Rules {
		protocol := rule.Protocol
		if protocol == "tcp" {
			protocol = "TCP"
		} else if protocol == "udp" {
			protocol = "UDP"
		} else if protocol == "icmp" {
			protocol = "ICMP"
		}

		if len(rule.Ports) == 0 {
			// All ports
			inboundRules = append(inboundRules, &linode.FirewallInboundArgs{
				Label:    pulumi.String(fmt.Sprintf("allow-%s-all", protocol)),
				Action:   pulumi.String("ACCEPT"),
				Protocol: pulumi.String(protocol),
				Ports:    pulumi.String("1-65535"),
				Ipv4s:    pulumi.ToStringArray(rule.Sources),
			})
		} else {
			// Specific ports
			for _, port := range rule.Ports {
				inboundRules = append(inboundRules, &linode.FirewallInboundArgs{
					Label:    pulumi.String(fmt.Sprintf("allow-%s-%d", protocol, port)),
					Action:   pulumi.String("ACCEPT"),
					Protocol: pulumi.String(protocol),
					Ports:    pulumi.String(fmt.Sprintf("%d", port)),
					Ipv4s:    pulumi.ToStringArray(rule.Sources),
				})
			}
		}
	}

	// Default outbound - allow all
	outboundRules := linode.FirewallOutboundArray{
		&linode.FirewallOutboundArgs{
			Label:    pulumi.String("allow-all-outbound"),
			Action:   pulumi.String("ACCEPT"),
			Protocol: pulumi.String("TCP"),
			Ports:    pulumi.String("1-65535"),
			Ipv4s:    pulumi.StringArray{pulumi.String("0.0.0.0/0")},
		},
		&linode.FirewallOutboundArgs{
			Label:    pulumi.String("allow-all-outbound-udp"),
			Action:   pulumi.String("ACCEPT"),
			Protocol: pulumi.String("UDP"),
			Ports:    pulumi.String("1-65535"),
			Ipv4s:    pulumi.StringArray{pulumi.String("0.0.0.0/0")},
		},
		&linode.FirewallOutboundArgs{
			Label:    pulumi.String("allow-icmp-outbound"),
			Action:   pulumi.String("ACCEPT"),
			Protocol: pulumi.String("ICMP"),
			Ipv4s:    pulumi.StringArray{pulumi.String("0.0.0.0/0")},
		},
	}

	// Convert server IDs to int array for Linode
	linodeIDs := pulumi.IntArray{}
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
				linodeIDs = append(linodeIDs, intOut)
			}
		}
	}

	fw, err := linode.NewFirewall(ctx, fwName, &linode.FirewallArgs{
		Label:           pulumi.String(fwName),
		Inbounds:        inboundRules,
		Outbounds:       outboundRules,
		InboundPolicy:   pulumi.String("DROP"),
		OutboundPolicy:  pulumi.String("ACCEPT"),
		Linodes:         linodeIDs,
	})
	if err != nil {
		return fmt.Errorf("failed to create firewall: %w", err)
	}

	ctx.Export("firewall_id", fw.ID())
	ctx.Export("firewall_name", fw.Label)

	return nil
}

// ValidateConfig validates Linode-specific configuration
func (p *LinodeProvider) ValidateConfig(infra *config.InfrastructureConfig) error {
	if infra.Credentials.Token == "" {
		return fmt.Errorf("Linode requires credentials.token (or LINODE_TOKEN env var)")
	}
	if infra.Region == "" {
		return fmt.Errorf("Linode requires a region (e.g., us-east, us-west, eu-west)")
	}
	return nil
}

// GetDefaultImage returns the default image for Linode
func (p *LinodeProvider) GetDefaultImage() string {
	return "linode/ubuntu22.04"
}

// GetDefaultSize returns the default instance type for Linode
func (p *LinodeProvider) GetDefaultSize() string {
	return "g6-nanode-1" // Smallest Linode instance (1GB)
}

// mapSize converts generic Tako sizes to Linode types
func (p *LinodeProvider) mapSize(size string) string {
	sizeMap := map[string]string{
		"small":  "g6-nanode-1",  // 1GB RAM
		"medium": "g6-standard-1", // 2GB RAM
		"large":  "g6-standard-2", // 4GB RAM
		"xlarge": "g6-standard-4", // 8GB RAM
	}

	if mapped, ok := sizeMap[size]; ok {
		return mapped
	}
	return size // Return as-is if it's already a Linode type
}
