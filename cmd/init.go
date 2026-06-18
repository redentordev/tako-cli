package cmd

import (
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/syscheck"
	"github.com/spf13/cobra"
)

var (
	useJSON bool
)

var initCmd = &cobra.Command{
	Use:   "init [project-name]",
	Short: "Initialize a new Tako CLI project",
	Long: `Initialize a new Tako CLI project by creating a tako.yaml (or tako.json) file
with example configuration. This is the first step in setting up deployments.

Use --json to generate tako.json instead of tako.yaml.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().BoolVar(&useJSON, "json", false, "Generate tako.json instead of tako.yaml")
}

func runInit(cmd *cobra.Command, args []string) error {
	projectName := "my-app"
	if len(args) > 0 {
		projectName = args[0]
	}

	// Check system requirements
	checker := syscheck.NewSystemChecker(verbose)
	result := checker.CheckAll()
	checker.PrintResults(result)

	// Check if Docker daemon is running
	if dockerRunning, msg := checker.CheckDocker(); !dockerRunning {
		fmt.Printf("\n⚠️  Warning: %s\n", msg)
		fmt.Println("Docker is required when Tako needs to build images locally.")
	}

	// Warn if required dependencies are missing
	if !result.AllRequired {
		fmt.Println("\n⚠️  Warning: Some required dependencies are missing.")
		fmt.Println("You can still initialize the project, but you'll need to install them before deploying.")
		fmt.Println()
	}

	// Offer to install Nixpacks if not present
	nixpacksInstalled := false
	for _, req := range result.Requirements {
		if req.Name == "Nixpacks" && req.Installed {
			nixpacksInstalled = true
			break
		}
	}

	if !nixpacksInstalled {
		fmt.Println("\n💡 Nixpacks is optional but recommended for best experience.")
		fmt.Println("It allows you to deploy without writing a Dockerfile - just push your code!")

		if !checker.CanInstallNixpacks() {
			fmt.Println("Install it manually later: https://nixpacks.com/docs/install")
		} else if checker.PromptNixpacksInstall() {
			if err := checker.InstallNixpacks(); err != nil {
				fmt.Printf("\n⚠️  Failed to install Nixpacks: %v\n", err)
				fmt.Println("You can install it manually later: https://nixpacks.com/docs/install")
			} else {
				fmt.Println("\n✓ Nixpacks is now available!")
			}
		} else {
			fmt.Println("\n  Skipping Nixpacks installation.")
			fmt.Println("  You can install it later with: https://nixpacks.com/docs/install")
		}
	}

	// Determine config file path based on format
	configPath := "tako.yaml"
	if useJSON {
		configPath = "tako.json"
	}

	// Check if config file already exists (check both formats)
	if _, err := os.Stat("tako.yaml"); err == nil {
		return fmt.Errorf("tako.yaml already exists. Remove it first or use a different directory")
	}
	if _, err := os.Stat("tako.json"); err == nil {
		return fmt.Errorf("tako.json already exists. Remove it first or use a different directory")
	}

	// Generate config content based on format
	var configContent string
	if useJSON {
		configContent = generateJSONConfig(projectName)
	} else {
		configContent = generateYAMLConfig(projectName)
	}

	// Write the config file
	if err := fileutil.WriteFileAtomic(configPath, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	fmt.Printf("\n✓ Created %s\n", configPath)

	// Create .env.example
	envExampleContent := fmt.Sprintf(`# 🐙 Tako CLI Environment Variables
# Copy this file to .env and fill in the values
# Run: cp .env.example .env

# Server Configuration
SERVER_HOST=your.server.ip

# Let's Encrypt Email (required for HTTPS)
LETSENCRYPT_EMAIL=you@example.com

# Database (if using PostgreSQL)
# POSTGRES_PASSWORD=your-secure-password
# DATABASE_URL=postgresql://user:password@postgres:5432/%s
`, projectName)

	if err := fileutil.WriteFileAtomic(".env.example", []byte(envExampleContent), 0644); err != nil {
		return fmt.Errorf("failed to write .env.example: %w", err)
	}
	fmt.Println("✓ Created .env.example")

	// Create .gitignore if it doesn't exist
	gitignorePath := ".gitignore"
	gitignoreContent := `# Tako CLI
.tako/
.env
*.pem
*.key

# IDE
.idea/
.vscode/
*.swp
*.swo

# OS
.DS_Store
Thumbs.db
`
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		if err := fileutil.WriteFileAtomic(gitignorePath, []byte(gitignoreContent), 0644); err != nil {
			return fmt.Errorf("failed to write .gitignore: %w", err)
		}
		fmt.Println("✓ Created .gitignore")
	}

	fmt.Println("\n🐙 Tako project initialized!")
	for _, line := range initNextSteps(configPath) {
		fmt.Println(line)
	}

	return nil
}

func initNextSteps(configPath string) []string {
	return []string{
		"",
		"Next steps:",
		"  1. Copy .env.example to .env and fill in your values",
		fmt.Sprintf("  2. Edit %s to configure your services", configPath),
		"  3. Commit your app and config changes",
		"  4. Run 'tako setup -e production' once per server",
		"  5. Run 'tako deploy -e production' to deploy",
	}
}

// generateJSONConfig creates a JSON configuration with schema reference
func generateJSONConfig(projectName string) string {
	return fmt.Sprintf(`{
  "$schema": "https://raw.githubusercontent.com/redentordev/tako-cli/master/schema/tako.schema.json",
  "project": {
    "name": "%s",
    "version": "1.0.0"
  },
  "runtime": {
    "mode": "takod",
    "proxy": "tako-proxy",
    "agent": {
      "enabled": true,
      "socket": "/run/tako/takod.sock",
      "dataDir": "/var/lib/tako"
    }
  },
  "state": {
    "backend": "replicated",
    "deployConsistency": "lease",
    "onUnreachableNode": "block",
    "remoteCacheEnabled": true
  },
  "mesh": {
    "enabled": true,
    "networkCIDR": "10.210.0.0/16",
    "interface": "tako",
    "listenPort": 51820,
    "subnetBits": 24,
    "natTraversal": true
  },
  "servers": {
    "production": {
      "host": "${SERVER_HOST}",
      "user": "root",
      "port": 22,
      "sshKey": "~/.ssh/id_ed25519"
    }
  },
  "environments": {
    "production": {
      "servers": ["production"],
      "services": {
        "web": {
          "build": ".",
          "port": 3000,
          "proxy": {
            "domain": "%s.${SERVER_HOST}.sslip.io",
            "email": "${LETSENCRYPT_EMAIL}"
          },
          "env": {
            "NODE_ENV": "production"
          }
        }
      }
    }
  }
}
`, projectName, projectName)
}

// generateYAMLConfig creates a YAML configuration (moved from inline)
func generateYAMLConfig(projectName string) string {
	return fmt.Sprintf(`# 🐙 Tako CLI Configuration
# Complete reference with all available options
# Uncomment and customize options as needed
# Learn more: https://github.com/redentordev/tako-cli

# ============================================================================
# PROJECT CONFIGURATION (Required)
# ============================================================================
project:
  name: %s
  version: 1.0.0

# ============================================================================
# RUNTIME MODEL
# ============================================================================
runtime:
  mode: takod
  proxy: tako-proxy
  agent:
    enabled: true
    socket: /run/tako/takod.sock
    dataDir: /var/lib/tako

state:
  backend: replicated        # Remote takod state is the source of truth.
  deployConsistency: lease   # Current deploy consistency policy.
  onUnreachableNode: block   # Block deploys when a selected node is unreachable.
  remoteCacheEnabled: true

mesh:
  enabled: true              # Single-node deployments are one-node meshes.
  networkCIDR: 10.210.0.0/16
  interface: tako
  listenPort: 51820
  subnetBits: 24
  natTraversal: true

# ============================================================================
# SERVER DEFINITIONS (Required)
# ============================================================================
servers:
  production:
    host: ${SERVER_HOST}       # Your VPS IP address or hostname
    user: root                 # SSH user (root recommended for setup)
    port: 22                   # SSH port (default: 22)
    sshKey: ~/.ssh/id_ed25519  # Path to your SSH private key
    # labels:                  # Optional: labels for placement/server selection
    #   zone: primary

# ============================================================================
# ENVIRONMENTS (Required)
# ============================================================================
environments:
  production:
    servers:
      - production
    # Optional: keep public ingress on one edge node in multi-node setups.
    # Built-in ACME TLS requires a single proxy node until distributed
    # certificate handling is implemented.
    # proxy:
    #   placement:
    #     constraints:
    #       - node.labels.role==edge
    services:
      # ======================================================================
      # WEB SERVICE - Basic web application
      # ======================================================================
      web:
        build: .          # Path to build context (directory with Dockerfile)
        # image: node:20  # Or use a pre-built image instead of build
        port: 3000        # Internal container port
        # replicas: 1     # Number of container replicas (default: 1)
        # restart: unless-stopped  # Restart policy: no, on-failure, always, unless-stopped
        
        # Public proxy routing
        proxy:
          # Primary explicit hostname where traffic is served.
          # Wildcard hostnames such as *.example.com are not supported yet.
          domain: %s.${SERVER_HOST}.sslip.io  # sslip.io provides automatic DNS
          
          # Domain redirects (301 redirect to primary domain with path preservation)
          # redirectFrom:
          #   - www.example.com      # www -> non-www
          #   - old.example.com      # old domain -> new domain

          # Dynamic customer domains for CMS/site-renderer apps.
          # Caddy asks this internal service/path before issuing a certificate.
          # dynamicDomains:
          #   ask: admin:/api/domains/authorize
          
          email: ${LETSENCRYPT_EMAIL}     # Email for Let's Encrypt SSL certificates
          # port: 3000      # Optional: specify if different from service port
        
        # Environment variables
        env:
          NODE_ENV: production
          # DATABASE_URL: postgresql://postgres:5432/myapp
        
        # Secrets (stored in .tako/secrets/)
        # secrets:
        #   - DATABASE_URL        # Load from secrets storage
        #   - JWT_SECRET
        #   - API_KEY:STRIPE_KEY  # Alias: container sees API_KEY, reads STRIPE_KEY
        
        # Volumes (persistent data)
        # volumes:
        #   - uploads:/app/uploads              # Named volume
        #   - /host/path:/container/path        # Bind mount
        
        # Health checks
        # healthCheck:
        #   path: /health
        #   interval: 10s
        #   timeout: 5s
        #   retries: 3
        
        # Cross-project service imports
        # imports:
        #   - other-project.postgres  # Provides other-project-production-postgres DNS
        
        # Export service to other projects
        # export: true

      # ======================================================================
      # DATABASE SERVICE - PostgreSQL example
      # ======================================================================
      # postgres:
      #   image: postgres:16-alpine
      #   port: 5432
      #   persistent: true  # Requires a volume; broad --force skips persistent services
      #   volumes:
      #     - postgres_data:/var/lib/postgresql/data
      #   # Required in multi-node environments so data has a known home.
      #   # placement:
      #   #   strategy: pinned
      #   #   servers: [server1]
      #   env:
      #     POSTGRES_USER: myapp
      #     POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      #     POSTGRES_DB: myapp_production
      #   # export: true  # Allow other projects to import this service

      # ======================================================================
      # CACHE SERVICE - Redis example
      # ======================================================================
      # redis:
      #   image: redis:7-alpine
      #   port: 6379
      #   persistent: true
      #   volumes:
      #     - redis_data:/data
      #   # placement:
      #   #   strategy: pinned
      #   #   servers: [server1]

      # ======================================================================
      # WORKER SERVICE - Background jobs example
      # ======================================================================
      # worker:
      #   build: ./worker
      #   # No port = background service (not publicly accessible)
      #   replicas: 2
      #   env:
      #     REDIS_URL: redis://redis:6379
      #     QUEUE_NAME: default

# ============================================================================
# DEPLOYMENT CONFIGURATION (Optional)
# ============================================================================
# deployment:
#   strategy: parallel      # parallel or sequential
#   parallel:
#     maxConcurrentBuilds: 4   # Max simultaneous builds
#     maxConcurrentDeploys: 4  # Max simultaneous deploys
#   cache:
#     enabled: true         # Enable Docker build cache
#     type: local           # Cache type: local

# ============================================================================
# DOCKER REGISTRY (Optional - for private images)
# ============================================================================
# registry:
#   url: ghcr.io
#   username: ${REGISTRY_USER}
#   password: ${REGISTRY_TOKEN}

# ============================================================================
	# MULTI-SERVER MESH CONFIGURATION (Optional - for 2+ servers)
	# ============================================================================
	# Additional servers join the same takod mesh model.
# 
# servers:
#   server1:
#     host: ${SERVER1_HOST}
#     user: root
#     sshKey: ~/.ssh/id_ed25519
#   server2:
#     host: ${SERVER2_HOST}
#     user: root
#     sshKey: ~/.ssh/id_ed25519
#
# environments:
#   production:
#     servers: [server1, server2]
#     # Required for public services with built-in ACME TLS on multi-node setups.
#     proxy:
#       placement:
#         strategy: pinned
#         servers: [server1]
#     services:
#       web:
#         build: .
#         replicas: 4
#         placement:
#           strategy: spread  # Distribute across all servers
#           # strategy: pinned  # Pin to specific servers
#           # servers: [server1]
#           # strategy: any     # No placement preference

# ============================================================================
# NOTIFICATIONS (Optional - deployment notifications)
# ============================================================================
# Get notified on Slack, Discord, or custom webhook when deployments start/finish
# notifications:
#   slack: ${SLACK_WEBHOOK_URL}       # Slack incoming webhook URL
#   discord: ${DISCORD_WEBHOOK_URL}   # Discord webhook URL  
#   webhook: https://your-endpoint.com/deploy  # Generic webhook (receives JSON)

# ============================================================================
# REFERENCE: Common Service Patterns
# ============================================================================

# Next.js App with www redirect:
# web:
#   build: .
#   port: 3000
#   proxy:
#     domain: app.example.com
#     redirectFrom:
#       - www.app.example.com
#     email: admin@example.com
#   env:
#     NODE_ENV: production
#     DATABASE_URL: postgresql://postgres:5432/myapp

# Laravel App:
# api:
#   build: .
#   port: 8000
# Python/Django:
# api:
#   build: .
#   port: 8000

# Full-stack with database:
# web:
#   build: ./frontend
#   port: 3000
#   proxy:
#     domain: app.example.com
# api:
#   build: ./backend
#   port: 4000
#   env:
#     DATABASE_URL: postgresql://postgres:5432/myapp
# postgres:
#   image: postgres:16-alpine
#   volumes:
#     - postgres_data:/var/lib/postgresql/data
#   env:
#     POSTGRES_PASSWORD: ${DB_PASSWORD}
`, projectName, projectName)
}
