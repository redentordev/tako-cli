package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/infra"
	"github.com/redentordev/tako-cli/pkg/syscheck"
	"github.com/spf13/cobra"
)

var (
	withInfra    bool
	showPricing  bool
	useJSON      bool
)

var initCmd = &cobra.Command{
	Use:   "init [project-name]",
	Short: "Initialize a new Tako CLI project",
	Long: `Initialize a new Tako CLI project by creating a tako.yaml (or tako.json) file
with example configuration. This is the first step in setting up deployments.

Use --json to generate tako.json instead of tako.yaml.

Use --infra flag to launch an interactive wizard that helps you:
  - Select a cloud provider (DigitalOcean, Hetzner, AWS, Linode)
  - Choose server sizes and regions
  - Configure networking (VPC, firewall)
  - See estimated monthly costs

Use --pricing to see a pricing comparison across all providers.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
	initCmd.Flags().BoolVar(&withInfra, "infra", false, "Use interactive wizard to configure cloud infrastructure")
	initCmd.Flags().BoolVar(&showPricing, "pricing", false, "Show pricing comparison across providers")
	initCmd.Flags().BoolVar(&useJSON, "json", false, "Generate tako.json instead of tako.yaml")
}

func runInit(cmd *cobra.Command, args []string) error {
	// Show pricing comparison if requested
	if showPricing {
		infra.ShowPricingComparison()
		infra.ShowStoragePricing()
		return nil
	}

	projectName := "my-app"
	if len(args) > 0 {
		projectName = args[0]
	}

	// Run infrastructure wizard if requested
	if withInfra {
		return runInfraWizard(projectName)
	}

	// Check system requirements
	checker := syscheck.NewSystemChecker(verbose)
	result := checker.CheckAll()
	checker.PrintResults(result)

	// Check if Docker daemon is running
	if dockerRunning, msg := checker.CheckDocker(); !dockerRunning {
		fmt.Printf("\n⚠️  Warning: %s\n", msg)
		fmt.Println("Docker is required for local development and building images.")
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

		if checker.PromptNixpacksInstall() {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
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

	if err := os.WriteFile(".env.example", []byte(envExampleContent), 0644); err != nil {
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
		if err := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0644); err != nil {
			return fmt.Errorf("failed to write .gitignore: %w", err)
		}
		fmt.Println("✓ Created .gitignore")
	}

	fmt.Println("\n🐙 Tako project initialized!")
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Copy .env.example to .env and fill in your values")
	fmt.Printf("  2. Edit %s to configure your services\n", configPath)
	fmt.Println("  3. Run 'tako deploy production' to deploy")

	return nil
}

// generateJSONConfig creates a JSON configuration with schema reference
func generateJSONConfig(projectName string) string {
	return fmt.Sprintf(`{
  "$schema": "https://raw.githubusercontent.com/redentordev/tako-cli/master/schema/tako.schema.json",
  "project": {
    "name": "%s",
    "version": "1.0.0"
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
# SERVER DEFINITIONS (Required)
# ============================================================================
servers:
  production:
    host: ${SERVER_HOST}       # Your VPS IP address or hostname
    user: root                 # SSH user (root recommended for setup)
    port: 22                   # SSH port (default: 22)
    sshKey: ~/.ssh/id_ed25519  # Path to your SSH private key
    # roles:                   # Optional: server roles for multi-server deployments
    #   - web
    #   - worker

# ============================================================================
# ENVIRONMENTS (Required)
# ============================================================================
environments:
  production:
    servers:
      - production
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
        
        # Traefik reverse proxy (for public web services)
        proxy:
          # Primary domain where traffic is served
          domain: %s.${SERVER_HOST}.sslip.io  # sslip.io provides automatic DNS
          # Or use multiple domains:
          # domains:
          #   - app.example.com
          #   - api.example.com
          
          # Domain redirects (301 redirect to primary domain with path preservation)
          # redirectFrom:
          #   - www.example.com      # www -> non-www
          #   - old.example.com      # old domain -> new domain
          
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
        
        # Lifecycle hooks
        # hooks:
        #   preBuild:
        #     - "npm run generate-types"
        #   postBuild:
        #     - "docker scan {{IMAGE}}"
        #   preDeploy:
        #     - "echo 'Starting deployment...'"
        #   postDeploy:
        #     - "curl https://api.slack.com/webhook..."
        #   postStart:
        #     - "exec: npm run migrate"   # Run inside container
        #     - "exec: npm run seed"
        
        # Cross-project service imports
        # imports:
        #   - other-project.postgres  # Import postgres from other-project
        
        # Export service to other projects
        # export: true

      # ======================================================================
      # DATABASE SERVICE - PostgreSQL example
      # ======================================================================
      # postgres:
      #   image: postgres:16-alpine
      #   port: 5432
      #   persistent: true  # Mark as persistent to prevent accidental removal
      #   volumes:
      #     - postgres_data:/var/lib/postgresql/data
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
# MULTI-SERVER SWARM CONFIGURATION (Optional - for 2+ servers)
# ============================================================================
# For deployments with multiple servers, Tako automatically uses Docker Swarm
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
#   hooks:
#     postStart:
#       - "exec: php artisan migrate --force"
#       - "exec: php artisan cache:clear"

# Python/Django:
# api:
#   build: .
#   port: 8000
#   hooks:
#     postStart:
#       - "exec: python manage.py migrate"
#       - "exec: python manage.py collectstatic --noinput"

# Full-stack with database:
# web:
#   build: ./frontend
#   port: 3000
#   proxy:
#     domains: [app.example.com]
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

// runInfraWizard runs the interactive infrastructure configuration wizard
func runInfraWizard(projectName string) error {
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

	// Run the wizard
	wizard := infra.NewWizard()
	result, err := wizard.Run()
	if err != nil {
		return fmt.Errorf("wizard failed: %w", err)
	}

	// Show summary
	fmt.Println("\n📋 Configuration Summary")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("Provider: %s\n", result.Config.Provider)
	fmt.Printf("Region: %s\n", result.Config.Region)
	fmt.Printf("Estimated monthly cost: %s\n", infra.FormatPricing(result.EstimatedCost))
	fmt.Println()

	// Generate full config based on format
	var configContent string
	if useJSON {
		// For JSON, we'd need to convert the infra YAML to JSON
		// For now, show a message that infra wizard only supports YAML
		return fmt.Errorf("--json flag is not yet supported with --infra wizard. Use 'tako init --json' without --infra")
	}
	configContent = generateFullConfig(projectName, result.YAMLContent)

	// Write config file
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	// Create .env.example with provider-specific content
	envExample := generateInfraEnvExample(result.Config.Provider, result.ProviderEnvVar)
	if err := os.WriteFile(".env.example", []byte(envExample), 0644); err != nil {
		fmt.Printf("Warning: Failed to create .env.example: %v\n", err)
	}

	// Create .gitignore
	createGitignore()

	absPath, _ := filepath.Abs(configPath)
	fmt.Printf("✓ Created %s at %s\n", filepath.Base(configPath), absPath)
	fmt.Printf("✓ Created .env.example\n")

	fmt.Printf("\n🚀 Next steps:\n")
	fmt.Printf("  1. Set %s environment variable\n", result.ProviderEnvVar)
	fmt.Printf("  2. Run 'tako provision' to create cloud servers\n")
	fmt.Printf("  3. Run 'tako setup' to configure Docker and Traefik\n")
	fmt.Printf("  4. Run 'tako deploy' to deploy your application\n")
	fmt.Println()
	fmt.Printf("💡 Or run 'tako deploy --full' for the complete lifecycle\n")

	return nil
}

// generateFullConfig creates the complete tako.yaml with infrastructure section
func generateFullConfig(projectName, infraYAML string) string {
	var sb strings.Builder

	sb.WriteString("# 🐙 Tako CLI Configuration\n")
	sb.WriteString("# Generated with: tako init --infra\n")
	sb.WriteString("# Learn more: https://github.com/redentordev/tako-cli\n")
	sb.WriteString("\n")

	// Project section
	sb.WriteString("project:\n")
	sb.WriteString(fmt.Sprintf("  name: %s\n", projectName))
	sb.WriteString("  version: 1.0.0\n")
	sb.WriteString("\n")

	// Infrastructure section (from wizard)
	sb.WriteString(infraYAML)
	sb.WriteString("\n")

	// Servers section (auto-populated after provisioning)
	sb.WriteString("# servers: (auto-populated after 'tako provision')\n")
	sb.WriteString("#   manager:\n")
	sb.WriteString("#     host: <provisioned-ip>\n")
	sb.WriteString("#     user: root\n")
	sb.WriteString("#     sshKey: ~/.ssh/id_ed25519\n")
	sb.WriteString("#     role: manager\n")
	sb.WriteString("\n")

	// Environments section
	sb.WriteString("environments:\n")
	sb.WriteString("  production:\n")
	sb.WriteString("    servers:\n")
	sb.WriteString("      - manager\n")
	sb.WriteString("    services:\n")
	sb.WriteString("      web:\n")
	sb.WriteString("        build: .\n")
	sb.WriteString("        port: 3000\n")
	sb.WriteString("        proxy:\n")
	sb.WriteString(fmt.Sprintf("          domain: %s.example.com\n", projectName))
	sb.WriteString("          email: ${LETSENCRYPT_EMAIL}\n")
	sb.WriteString("        env:\n")
	sb.WriteString("          NODE_ENV: production\n")

	return sb.String()
}

// generateInfraEnvExample creates .env.example with provider-specific variables
func generateInfraEnvExample(provider, envVar string) string {
	var sb strings.Builder

	sb.WriteString("# 🐙 Tako CLI Environment Variables\n")
	sb.WriteString("# Copy this file to .env and fill in your actual values\n")
	sb.WriteString("\n")

	sb.WriteString("# ============================================================================\n")
	sb.WriteString("# CLOUD PROVIDER CREDENTIALS (Required for infrastructure provisioning)\n")
	sb.WriteString("# ============================================================================\n")

	switch provider {
	case "digitalocean":
		sb.WriteString("# Get your token at: https://cloud.digitalocean.com/account/api/tokens\n")
		sb.WriteString("DIGITALOCEAN_TOKEN=your-api-token-here\n")
	case "hetzner":
		sb.WriteString("# Get your token at: https://console.hetzner.cloud/projects/*/security/tokens\n")
		sb.WriteString("HCLOUD_TOKEN=your-api-token-here\n")
	case "aws":
		sb.WriteString("# Get your keys at: https://console.aws.amazon.com/iam/home#/security_credentials\n")
		sb.WriteString("AWS_ACCESS_KEY_ID=your-access-key-id\n")
		sb.WriteString("AWS_SECRET_ACCESS_KEY=your-secret-access-key\n")
	case "linode":
		sb.WriteString("# Get your token at: https://cloud.linode.com/profile/tokens\n")
		sb.WriteString("LINODE_TOKEN=your-api-token-here\n")
	}

	sb.WriteString("\n")
	sb.WriteString("# ============================================================================\n")
	sb.WriteString("# SSL CERTIFICATE (Required for HTTPS)\n")
	sb.WriteString("# ============================================================================\n")
	sb.WriteString("LETSENCRYPT_EMAIL=admin@example.com\n")
	sb.WriteString("\n")

	sb.WriteString("# ============================================================================\n")
	sb.WriteString("# APPLICATION SECRETS (Optional)\n")
	sb.WriteString("# ============================================================================\n")
	sb.WriteString("# DATABASE_URL=postgresql://user:password@postgres:5432/myapp\n")
	sb.WriteString("# JWT_SECRET=your-jwt-secret-here\n")
	sb.WriteString("# API_KEY=your-api-key-here\n")

	return sb.String()
}

// createGitignore creates a .gitignore file if it doesn't exist
func createGitignore() {
	gitignorePath := ".gitignore"
	gitignoreContent := `.env
.env.local
.tako/
*.log
dist/
build/
node_modules/
`

	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		if err := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0644); err != nil {
			fmt.Printf("Warning: Failed to create .gitignore: %v\n", err)
		}
	}
}
