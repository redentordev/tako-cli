# Infrastructure Provisioning

Provision cloud servers with one command. Tako handles VPC, firewall, and SSH keys automatically.

## Supported Providers

| Provider | Token |
|----------|-------|
| DigitalOcean | `DIGITALOCEAN_TOKEN` |
| Hetzner | `HCLOUD_TOKEN` |
| Linode | `LINODE_TOKEN` |
| AWS | `AWS_ACCESS_KEY_ID` + `AWS_SECRET_ACCESS_KEY` |

## Quick Start

```bash
# 1. Set your token
export HCLOUD_TOKEN="your-token-here"

# 2. Add infrastructure to tako.yaml
# 3. Provision and deploy
tako provision
tako setup
tako deploy
```

## Configuration

```yaml
infrastructure:
  provider: hetzner
  region: fsn1
  credentials:
    token: ${HCLOUD_TOKEN}
  servers:
    web:
      size: cax11
      role: manager
```

That's it. Tako auto-generates SSH keys and configures networking.

## Commands

```bash
tako provision            # Create servers
tako provision --preview  # Preview changes
tako infra outputs        # Show server IPs
tako infra destroy        # Tear down
```

---

## Provider Reference

### DigitalOcean

```yaml
infrastructure:
  provider: digitalocean
  region: nyc1
  credentials:
    token: ${DIGITALOCEAN_TOKEN}
  servers:
    web:
      size: s-1vcpu-1gb
      role: manager
```

**Regions:** `nyc1` `nyc3` `sfo3` `ams3` `sgp1` `lon1` `fra1`

**Sizes:** `s-1vcpu-1gb` `s-1vcpu-2gb` `s-2vcpu-4gb` `s-4vcpu-8gb`

Get token: [cloud.digitalocean.com/account/api/tokens](https://cloud.digitalocean.com/account/api/tokens)

---

### Hetzner

```yaml
infrastructure:
  provider: hetzner
  region: fsn1
  credentials:
    token: ${HCLOUD_TOKEN}
  servers:
    web:
      size: cax11
      role: manager
```

**Regions:** `fsn1` `nbg1` `hel1` `ash` `hil`

**Sizes (ARM):** `cax11` `cax21` `cax31` `cax41` ← Best value

**Sizes (x86):** `cx22` `cx32` `cx42` `cx52`

Get token: [console.hetzner.cloud](https://console.hetzner.cloud/) → Security → API Tokens

---

### Linode

```yaml
infrastructure:
  provider: linode
  region: us-east
  credentials:
    token: ${LINODE_TOKEN}
  servers:
    web:
      size: g6-nanode-1
      role: manager
```

**Regions:** `us-east` `us-west` `eu-west` `eu-central` `ap-south` `ap-northeast`

**Sizes:** `g6-nanode-1` `g6-standard-1` `g6-standard-2` `g6-standard-4`

Get token: [cloud.linode.com/profile/tokens](https://cloud.linode.com/profile/tokens)

---

### AWS

```yaml
infrastructure:
  provider: aws
  region: us-east-1
  credentials:
    accessKey: ${AWS_ACCESS_KEY_ID}
    secretKey: ${AWS_SECRET_ACCESS_KEY}
  servers:
    web:
      size: t3.micro
      role: manager
```

**Regions:** `us-east-1` `us-west-2` `eu-west-1` `eu-central-1` `ap-northeast-1`

**Sizes:** `t3.micro` `t3.small` `t3.medium` `t3.large`

Get keys: [console.aws.amazon.com/iam](https://console.aws.amazon.com/iam/) → Users → Security credentials

---

## Multi-Server Setup

```yaml
infrastructure:
  provider: hetzner
  region: fsn1
  credentials:
    token: ${HCLOUD_TOKEN}
  servers:
    manager:
      size: cax11
      role: manager
    workers:
      size: cax11
      role: worker
      count: 2          # Creates workers-0, workers-1

environments:
  production:
    servers: [manager, workers-0, workers-1]
```

## Full Options

```yaml
infrastructure:
  provider: hetzner
  region: fsn1
  credentials:
    token: ${HCLOUD_TOKEN}

  # SSH (optional - auto-generated if not set)
  ssh_key: ~/.ssh/id_ed25519
  ssh_user: root

  # Defaults for all servers
  defaults:
    size: cax11
    image: ubuntu-22.04

  servers:
    web:
      size: cax21
      role: manager
      tags: [web]

  # Networking (optional - enabled by default)
  networking:
    vpc:
      enabled: true
      ip_range: 10.0.0.0/16
    firewall:
      enabled: true
```

## Security

- Never commit tokens to git
- Use environment variables: `${HCLOUD_TOKEN}`
- Add `.env` to `.gitignore`
- Tako auto-loads `.env` files
