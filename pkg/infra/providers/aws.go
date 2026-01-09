package providers

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws"
	"github.com/pulumi/pulumi-aws/sdk/v6/go/aws/ec2"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/redentordev/tako-cli/pkg/config"
)

// AWSProvider implements the Provider interface for AWS
type AWSProvider struct {
	accessKey string
	secretKey string
	region    string
}

// NewAWS creates a new AWS provider
func NewAWS() Provider {
	return &AWSProvider{}
}

// Name returns the provider identifier
func (p *AWSProvider) Name() string {
	return "aws"
}

// Configure sets up the provider with credentials
func (p *AWSProvider) Configure(ctx *pulumi.Context, creds config.InfraCredentialsConfig) error {
	if creds.AccessKey == "" || creds.SecretKey == "" {
		return fmt.Errorf("AWS requires accessKey and secretKey credentials (set AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY env vars)")
	}
	p.accessKey = creds.AccessKey
	p.secretKey = creds.SecretKey

	// Create AWS provider instance
	_, err := aws.NewProvider(ctx, "aws-provider", &aws.ProviderArgs{
		AccessKey: pulumi.String(p.accessKey),
		SecretKey: pulumi.String(p.secretKey),
	})
	if err != nil {
		return fmt.Errorf("failed to create AWS provider: %w", err)
	}

	return nil
}

// CreateSSHKey imports an SSH public key to AWS and returns the key name
func (p *AWSProvider) CreateSSHKey(ctx *pulumi.Context, publicKey string, keyName string) (pulumi.StringOutput, error) {
	keyPair, err := ec2.NewKeyPair(ctx, keyName, &ec2.KeyPairArgs{
		KeyName:   pulumi.String(keyName),
		PublicKey: pulumi.String(publicKey),
		Tags: pulumi.StringMap{
			"Name": pulumi.String(keyName),
		},
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create key pair: %w", err)
	}

	ctx.Export("ssh_key_id", keyPair.KeyPairId)
	ctx.Export("ssh_key_name", keyPair.KeyName)
	ctx.Export("ssh_key_fingerprint", keyPair.Fingerprint)

	// AWS uses key name, not ID, for instances
	return keyPair.KeyName.ToStringOutput(), nil
}

// CreateServer provisions an AWS EC2 instance
func (p *AWSProvider) CreateServer(ctx *pulumi.Context, name string, spec config.InfraServerSpec, region string, vpcID pulumi.StringInput, resourceName string, autoSSHKeyID pulumi.StringOutput, autoSSHPublicKey string) ([]pulumi.StringInput, error) {
	// Default values
	instanceType := spec.Size
	if instanceType == "" {
		instanceType = p.GetDefaultSize()
	}

	// Map generic sizes to AWS instance types
	instanceType = p.mapSize(instanceType)

	ami := spec.Image
	if ami == "" {
		ami = p.GetDefaultImage()
	}

	count := spec.Count
	if count < 1 {
		count = 1
	}

	// Convert tags
	tags := make(map[string]string)
	for _, tag := range spec.Tags {
		tags[tag] = "true"
	}

	// Add role as a tag
	role := spec.Role
	if role == "" {
		role = "worker"
	}
	tags["role"] = role
	tags["Name"] = name

	// Collect server IDs for security group attachment
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

		instanceTags := make(map[string]string)
		for k, v := range tags {
			instanceTags[k] = v
		}
		instanceTags["Name"] = instanceName

		args := &ec2.InstanceArgs{
			Ami:          pulumi.String(ami),
			InstanceType: pulumi.String(instanceType),
			Tags:         pulumi.ToStringMap(instanceTags),
		}

		// Add key pair if specified (first key is AWS key pair name), or use auto-generated key
		if len(spec.SSHKeys) > 0 {
			args.KeyName = pulumi.String(spec.SSHKeys[0])
		} else if autoSSHKeyID != (pulumi.StringOutput{}) {
			// Use auto-generated SSH key (for AWS, this is the key name)
			args.KeyName = autoSSHKeyID
		}

		// Prepare user data - combine any existing user data with additional SSH keys
		userData := spec.UserData
		if len(spec.SSHKeys) > 1 {
			// AWS only supports one key pair per instance, so additional public keys
			// need to be added via cloud-init user_data
			// Note: For this to work, the additional keys should be SSH public key content (not key pair names)
			additionalKeys := spec.SSHKeys[1:]
			cloudInit := "#cloud-config\n"
			if userData != "" && !strings.HasPrefix(userData, "#cloud-config") {
				// Existing user_data is a script, we need to combine
				cloudInit += "runcmd:\n"
				cloudInit += "  - |\n"
				for _, line := range strings.Split(userData, "\n") {
					cloudInit += "    " + line + "\n"
				}
			}
			cloudInit += "ssh_authorized_keys:\n"
			for _, key := range additionalKeys {
				cloudInit += fmt.Sprintf("  - %s\n", key)
			}
			userData = cloudInit
		}

		// Add user data if specified
		if userData != "" {
			args.UserData = pulumi.String(userData)
		}

		// Add to VPC subnet if provided
		if vpcID != nil {
			if strOutput, ok := vpcID.(pulumi.StringOutput); ok {
				args.SubnetId = strOutput
			}
		}

		// Create the instance
		instance, err := ec2.NewInstance(ctx, instanceName, args)
		if err != nil {
			return nil, fmt.Errorf("failed to create instance %s: %w", instanceName, err)
		}

		// Collect server ID for security group
		serverIDs = append(serverIDs, instance.ID().ToStringOutput())

		// Export outputs
		ctx.Export(fmt.Sprintf("%s_id", configName), instance.ID())
		ctx.Export(fmt.Sprintf("%s_ip", configName), instance.PublicIp)
		ctx.Export(fmt.Sprintf("%s_private_ip", configName), instance.PrivateIp)
		ctx.Export(fmt.Sprintf("%s_status", configName), instance.InstanceState)
		ctx.Export(fmt.Sprintf("%s_region", configName), pulumi.String(region))
		ctx.Export(fmt.Sprintf("%s_role", configName), pulumi.String(role))
	}

	return serverIDs, nil
}

// CreateVPC provisions an AWS VPC with subnets
func (p *AWSProvider) CreateVPC(ctx *pulumi.Context, name string, spec *config.InfraVPCConfig, region string) (pulumi.StringOutput, error) {
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

	// Create the VPC
	vpc, err := ec2.NewVpc(ctx, vpcName, &ec2.VpcArgs{
		CidrBlock:          pulumi.String(ipRange),
		EnableDnsHostnames: pulumi.Bool(true),
		EnableDnsSupport:   pulumi.Bool(true),
		Tags: pulumi.StringMap{
			"Name": pulumi.String(vpcName),
		},
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create VPC: %w", err)
	}

	// Create an Internet Gateway
	igw, err := ec2.NewInternetGateway(ctx, fmt.Sprintf("%s-igw", vpcName), &ec2.InternetGatewayArgs{
		VpcId: vpc.ID(),
		Tags: pulumi.StringMap{
			"Name": pulumi.String(fmt.Sprintf("%s-igw", vpcName)),
		},
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create internet gateway: %w", err)
	}

	// Create a public subnet
	subnet, err := ec2.NewSubnet(ctx, fmt.Sprintf("%s-subnet", vpcName), &ec2.SubnetArgs{
		VpcId:               vpc.ID(),
		CidrBlock:           pulumi.String("10.0.1.0/24"),
		MapPublicIpOnLaunch: pulumi.Bool(true),
		Tags: pulumi.StringMap{
			"Name": pulumi.String(fmt.Sprintf("%s-subnet", vpcName)),
		},
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create subnet: %w", err)
	}

	// Create a route table
	routeTable, err := ec2.NewRouteTable(ctx, fmt.Sprintf("%s-rt", vpcName), &ec2.RouteTableArgs{
		VpcId: vpc.ID(),
		Routes: ec2.RouteTableRouteArray{
			&ec2.RouteTableRouteArgs{
				CidrBlock: pulumi.String("0.0.0.0/0"),
				GatewayId: igw.ID(),
			},
		},
		Tags: pulumi.StringMap{
			"Name": pulumi.String(fmt.Sprintf("%s-rt", vpcName)),
		},
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to create route table: %w", err)
	}

	// Associate route table with subnet
	_, err = ec2.NewRouteTableAssociation(ctx, fmt.Sprintf("%s-rta", vpcName), &ec2.RouteTableAssociationArgs{
		SubnetId:     subnet.ID(),
		RouteTableId: routeTable.ID(),
	})
	if err != nil {
		return pulumi.StringOutput{}, fmt.Errorf("failed to associate route table: %w", err)
	}

	ctx.Export("vpc_id", vpc.ID())
	ctx.Export("vpc_name", pulumi.String(vpcName))
	ctx.Export("vpc_cidr", vpc.CidrBlock)
	ctx.Export("subnet_id", subnet.ID())

	// Return subnet ID for instances to use
	return subnet.ID().ToStringOutput(), nil
}

// CreateFirewall provisions AWS Security Groups
func (p *AWSProvider) CreateFirewall(ctx *pulumi.Context, name string, spec *config.InfraFirewallConfig, serverIDs []pulumi.StringInput) error {
	if spec == nil || !spec.Enabled {
		return nil
	}

	sgName := spec.Name
	if sgName == "" {
		sgName = fmt.Sprintf("%s-sg", name)
	}

	// Build ingress rules
	ingressRules := ec2.SecurityGroupIngressArray{}
	for _, rule := range spec.Rules {
		if len(rule.Ports) == 0 {
			// All ports for this protocol
			ingressRules = append(ingressRules, &ec2.SecurityGroupIngressArgs{
				Protocol:   pulumi.String(rule.Protocol),
				FromPort:   pulumi.Int(0),
				ToPort:     pulumi.Int(65535),
				CidrBlocks: pulumi.ToStringArray(rule.Sources),
			})
		} else {
			// Specific ports
			for _, port := range rule.Ports {
				ingressRules = append(ingressRules, &ec2.SecurityGroupIngressArgs{
					Protocol:   pulumi.String(rule.Protocol),
					FromPort:   pulumi.Int(port),
					ToPort:     pulumi.Int(port),
					CidrBlocks: pulumi.ToStringArray(rule.Sources),
				})
			}
		}
	}

	// Default egress - allow all outbound
	egressRules := ec2.SecurityGroupEgressArray{
		&ec2.SecurityGroupEgressArgs{
			Protocol:   pulumi.String("-1"),
			FromPort:   pulumi.Int(0),
			ToPort:     pulumi.Int(0),
			CidrBlocks: pulumi.StringArray{pulumi.String("0.0.0.0/0")},
		},
	}

	sg, err := ec2.NewSecurityGroup(ctx, sgName, &ec2.SecurityGroupArgs{
		Name:        pulumi.String(sgName),
		Description: pulumi.String(fmt.Sprintf("Security group for %s", name)),
		Ingress:     ingressRules,
		Egress:      egressRules,
		Tags: pulumi.StringMap{
			"Name": pulumi.String(sgName),
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create security group: %w", err)
	}

	ctx.Export("security_group_id", sg.ID())
	ctx.Export("security_group_name", sg.Name)

	return nil
}

// ValidateConfig validates AWS-specific configuration
func (p *AWSProvider) ValidateConfig(infra *config.InfrastructureConfig) error {
	if infra.Credentials.AccessKey == "" {
		return fmt.Errorf("AWS requires credentials.accessKey (or AWS_ACCESS_KEY_ID env var)")
	}
	if infra.Credentials.SecretKey == "" {
		return fmt.Errorf("AWS requires credentials.secretKey (or AWS_SECRET_ACCESS_KEY env var)")
	}
	if infra.Region == "" {
		return fmt.Errorf("AWS requires a region (e.g., us-east-1, eu-west-1)")
	}
	return nil
}

// GetDefaultImage returns the default AMI for AWS (Ubuntu 22.04 in us-east-1)
// Note: AMIs are region-specific, this is for us-east-1
func (p *AWSProvider) GetDefaultImage() string {
	return "ami-0c7217cdde317cfec" // Ubuntu 22.04 LTS in us-east-1
}

// GetDefaultSize returns the default instance type for AWS
func (p *AWSProvider) GetDefaultSize() string {
	return "t3.micro"
}

// mapSize converts generic Tako sizes to AWS instance types
func (p *AWSProvider) mapSize(size string) string {
	sizeMap := map[string]string{
		"small":  "t3.micro",
		"medium": "t3.small",
		"large":  "t3.medium",
		"xlarge": "t3.large",
	}

	if mapped, ok := sizeMap[size]; ok {
		return mapped
	}
	return size // Return as-is if it's already an AWS instance type
}
