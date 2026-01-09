# Infrastructure Provisioning

Tako CLI can automatically provision cloud infrastructure before deploying your applications. This document covers provider setup, credential configuration, and usage.

## Supported Providers

| Provider | Environment Variable | Credential Type |
|----------|---------------------|-----------------|
| **DigitalOcean** | `DIGITALOCEAN_TOKEN` | API Token |
| **Hetzner Cloud** | `HCLOUD_TOKEN` | API Token |
| **Linode** | `LINODE_TOKEN` | API Token |
| **AWS** | `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` | Access Keys |

## Quick Start

1. Set your provider credentials as environment variables
2. Add an `infrastructure` section to your `tako.yaml`
3. Run `tako provision` to create servers
4. Run `tako setup` and `tako deploy` as usual

## Provider Setup

### DigitalOcean

1. **Get API Token:**
   - Go to [DigitalOcean API Tokens](https://cloud.digitalocean.com/account/api/tokens)
   - Click "Generate New Token"
   - Enable both Read and Write scopes
   - Copy the token (shown only once)

2. **Set Environment Variable:**
   ```bash
   export DIGITALOCEAN_TOKEN="dop_v1_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
   ```

3. **Configuration:**
   ```yaml
   infrastructure:
     provider: digitalocean
     region: nyc1  # nyc1, nyc3, sfo3, ams3, sgp1, lon1, fra1, etc.
     credentials:
       token: ${DIGITALOCEAN_TOKEN}
     servers:
       web:
         size: s-1vcpu-1gb  # or: s-2vcpu-4gb, s-4vcpu-8gb, etc.
         role: manager
   ```

4. **Available Regions:**
   - `nyc1`, `nyc3` - New York
   - `sfo3` - San Francisco
   - `ams3` - Amsterdam
   - `sgp1` - Singapore
   - `lon1` - London
   - `fra1` - Frankfurt
   - `blr1` - Bangalore
   - `tor1` - Toronto

5. **Server Sizes:**
   - `s-1vcpu-1gb` - 1 vCPU, 1GB RAM ($6/mo)
   - `s-1vcpu-2gb` - 1 vCPU, 2GB RAM ($12/mo)
   - `s-2vcpu-4gb` - 2 vCPU, 4GB RAM ($24/mo)
   - `s-4vcpu-8gb` - 4 vCPU, 8GB RAM ($48/mo)
   - See full list: `doctl compute size list`

---

### Hetzner Cloud

1. **Get API Token:**
   - Go to [Hetzner Cloud Console](https://console.hetzner.cloud/)
   - Select your project (or create one)
   - Go to Security → API Tokens
   - Click "Generate API Token"
   - Enable Read & Write permissions
   - Copy the token

2. **Set Environment Variable:**
   ```bash
   export HCLOUD_TOKEN="xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
   ```

3. **Configuration:**
   ```yaml
   infrastructure:
     provider: hetzner
     region: fsn1  # fsn1, nbg1, hel1, ash, hil
     credentials:
       token: ${HCLOUD_TOKEN}
     servers:
       web:
         size: cax11  # ARM-based, cost-effective
         role: manager
   ```

4. **Available Regions:**
   - `fsn1` - Falkenstein, Germany
   - `nbg1` - Nuremberg, Germany
   - `hel1` - Helsinki, Finland
   - `ash` - Ashburn, Virginia, USA
   - `hil` - Hillsboro, Oregon, USA

5. **Server Sizes (x86):**
   - `cx22` - 2 vCPU, 4GB RAM
   - `cx32` - 4 vCPU, 8GB RAM
   - `cx42` - 8 vCPU, 16GB RAM
   - `cx52` - 16 vCPU, 32GB RAM

6. **Server Sizes (ARM - Recommended for cost):**
   - `cax11` - 2 vCPU, 4GB RAM (default)
   - `cax21` - 4 vCPU, 8GB RAM
   - `cax31` - 8 vCPU, 16GB RAM
   - `cax41` - 16 vCPU, 32GB RAM

---

### Linode

1. **Get API Token:**
   - Go to [Linode API Tokens](https://cloud.linode.com/profile/tokens)
   - Click "Create a Personal Access Token"
   - Set expiry (or no expiry)
   - Enable Read/Write for: Linodes, Firewalls, VPCs, SSH Keys
   - Copy the token

2. **Set Environment Variable:**
   ```bash
   export LINODE_TOKEN="xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
   ```

3. **Configuration:**
   ```yaml
   infrastructure:
     provider: linode
     region: us-east  # us-east, us-west, eu-west, ap-south, etc.
     credentials:
       token: ${LINODE_TOKEN}
     servers:
       web:
         size: g6-nanode-1  # Smallest instance
         role: manager
   ```

4. **Available Regions:**
   - `us-east` - Newark, NJ
   - `us-central` - Dallas, TX
   - `us-west` - Fremont, CA
   - `us-southeast` - Atlanta, GA
   - `eu-west` - London, UK
   - `eu-central` - Frankfurt, DE
   - `ap-south` - Singapore
   - `ap-northeast` - Tokyo, JP
   - `ap-west` - Mumbai, IN

5. **Server Sizes:**
   - `g6-nanode-1` - 1 vCPU, 1GB RAM ($5/mo)
   - `g6-standard-1` - 1 vCPU, 2GB RAM ($10/mo)
   - `g6-standard-2` - 2 vCPU, 4GB RAM ($20/mo)
   - `g6-standard-4` - 4 vCPU, 8GB RAM ($40/mo)

---

### AWS (Amazon Web Services)

1. **Get Access Keys:**
   - Go to [AWS IAM Console](https://console.aws.amazon.com/iam/)
   - Go to Users → Your User → Security credentials
   - Click "Create access key"
   - Copy Access Key ID and Secret Access Key

2. **Required IAM Permissions:**
   ```json
   {
     "Version": "2012-10-17",
     "Statement": [
       {
         "Effect": "Allow",
         "Action": [
           "ec2:*",
           "vpc:*"
         ],
         "Resource": "*"
       }
     ]
   }
   ```

3. **Set Environment Variables:**
   ```bash
   export AWS_ACCESS_KEY_ID="AKIAXXXXXXXXXXXXXXXX"
   export AWS_SECRET_ACCESS_KEY="xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
   ```

4. **Configuration:**
   ```yaml
   infrastructure:
     provider: aws
     region: us-east-1
     credentials:
       accessKey: ${AWS_ACCESS_KEY_ID}
       secretKey: ${AWS_SECRET_ACCESS_KEY}
     servers:
       web:
         size: t3.micro  # or use generic: small, medium, large
         image: ami-0c7217cdde317cfec  # Ubuntu 22.04 in us-east-1
         role: manager
   ```

5. **Available Regions:**
   - `us-east-1` - N. Virginia
   - `us-east-2` - Ohio
   - `us-west-1` - N. California
   - `us-west-2` - Oregon
   - `eu-west-1` - Ireland
   - `eu-central-1` - Frankfurt
   - `ap-northeast-1` - Tokyo
   - `ap-southeast-1` - Singapore

6. **Server Sizes (Generic):**
   - `small` → `t3.micro`
   - `medium` → `t3.small`
   - `large` → `t3.medium`
   - `xlarge` → `t3.large`

7. **Note:** AWS AMIs are region-specific. The default AMI is for `us-east-1`. For other regions, specify the correct AMI ID for Ubuntu 22.04.

---

## Full Configuration Reference

```yaml
infrastructure:
  # Required: Cloud provider
  provider: hetzner  # digitalocean | hetzner | aws | linode

  # Required: Region/datacenter
  region: fsn1

  # Credentials (use environment variables for security)
  credentials:
    token: ${HCLOUD_TOKEN}        # For DO, Hetzner, Linode
    # OR for AWS:
    # accessKey: ${AWS_ACCESS_KEY_ID}
    # secretKey: ${AWS_SECRET_ACCESS_KEY}

  # Optional: SSH configuration
  ssh_key: ~/.ssh/id_ed25519     # Local SSH key for connecting
  ssh_user: root                  # SSH user (default: root)

  # Optional: Default values for all servers
  defaults:
    size: cax11
    image: ubuntu-22.04
    tags: [production]

  # Server definitions
  servers:
    web:
      size: cax11                 # Provider-specific or generic size
      image: ubuntu-22.04         # OS image (optional, has defaults)
      role: manager               # manager or worker
      count: 1                    # Number of instances (default: 1)
      tags: [web, frontend]       # Server tags/labels

    workers:
      size: cax21
      role: worker
      count: 2                    # Creates workers-0, workers-1

  # Optional: Networking
  networking:
    vpc:
      enabled: true
      ip_range: 10.0.0.0/16
    firewall:
      enabled: true
      rules:
        - protocol: tcp
          ports: [22, 80, 443]
          sources: [0.0.0.0/0]
        - protocol: tcp
          ports: [2377, 7946]     # Docker Swarm ports
          sources: [10.0.0.0/16]  # VPC only
```

## Commands

```bash
# Preview what will be created
tako provision --preview

# Create infrastructure
tako provision

# Show current infrastructure status
tako infra outputs

# Validate configuration
tako infra validate

# Destroy infrastructure
tako infra destroy
```

## Environment Variables Summary

| Provider | Required Variables |
|----------|-------------------|
| DigitalOcean | `DIGITALOCEAN_TOKEN` |
| Hetzner | `HCLOUD_TOKEN` |
| Linode | `LINODE_TOKEN` |
| AWS | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |

## Security Best Practices

1. **Never commit credentials** to version control
2. **Use environment variables** instead of hardcoding in `tako.yaml`
3. **Use `.env` files** for local development (add to `.gitignore`)
4. **Rotate tokens regularly** in production
5. **Use minimal permissions** - only grant what's needed

Example `.env` file:
```bash
# .env (add to .gitignore!)
HCLOUD_TOKEN=your-token-here
```

Tako automatically loads `.env` files and expands `${VAR}` syntax in `tako.yaml`.

## Auto-Generated SSH Keys

If you don't specify SSH keys, Tako automatically:
1. Generates an ED25519 SSH key pair
2. Stores it in `.tako/ssh/`
3. Uploads the public key to your cloud provider
4. Uses it for server access

The key is stored at:
- Private: `.tako/ssh/tako-{project}.pem`
- Public: `.tako/ssh/tako-{project}.pub`

## Troubleshooting

### "Token not found" Error
```
Hetzner requires a token (set credentials.token or HCLOUD_TOKEN env var)
```
**Solution:** Set the environment variable or add credentials to tako.yaml.

### "Invalid token" Error
**Solution:** Verify your token is valid and has read/write permissions.

### "Region not found" Error
**Solution:** Check the region name against the provider's available regions listed above.

### "Size not found" Error
**Solution:** Use `tako infra validate` to see available sizes for your provider.
