package cmd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/redentordev/tako-cli/pkg/syscheck"
	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init [project-name]",
	Short: "Initialize a new Tako CLI project",
	Long: `Initialize a new Tako CLI project by creating a tako.yaml file
with example configuration. This is the first step in setting up deployments.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runInit,
}

func init() {
	rootCmd.AddCommand(initCmd)
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

	// Check if config file already exists
	configPath := "tako.yaml"
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("tako.yaml already exists. Remove it first or use a different directory")
	}

	// Create comprehensive example config with all options
	configContent := fmt.Sprintf(`# 🐙 Tako CLI Configuration
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

	// Write config file
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		return fmt.Errorf("failed to create config file: %w", err)
	}

	// Create .env.example file
	envExample := `# 🐙 Tako CLI Environment Variables
# Copy this file to .env and fill in your actual values
# IMPORTANT: Never commit .env to version control!

# ============================================================================
# SERVER CONFIGURATION (Required)
# ============================================================================
# Your VPS server IP address or hostname
SERVER_HOST=203.0.113.10

# For multi-server deployments, add additional server hosts:
# SERVER1_HOST=203.0.113.11
# SERVER2_HOST=203.0.113.12

# ============================================================================
# SSL CERTIFICATE (Required for HTTPS)
# ============================================================================
# Email address for Let's Encrypt SSL certificate notifications
LETSENCRYPT_EMAIL=admin@example.com

# ============================================================================
# SECRETS (Optional - recommended to use 'tako secrets' command instead)
# ============================================================================
# For better security, use: tako secrets init && tako secrets set KEY=value
# These are shown here for reference only

# Database connection strings
# DATABASE_URL=postgresql://user:password@postgres:5432/myapp
# POSTGRES_PASSWORD=your-secure-password-here
# REDIS_URL=redis://redis:6379

# Application secrets
# JWT_SECRET=your-jwt-secret-here
# API_KEY=your-api-key-here
# STRIPE_SECRET_KEY=sk_live_...

# ============================================================================
# DOCKER REGISTRY (Optional - for private registries)
# ============================================================================
# Required only if using private Docker registries (GitHub, GitLab, Docker Hub, etc.)
# REGISTRY_USER=your-username
# REGISTRY_TOKEN=your-personal-access-token

# ============================================================================
# APPLICATION ENVIRONMENT VARIABLES
# ============================================================================
# Add your application-specific environment variables here
# NODE_ENV=production
# PORT=3000
# LOG_LEVEL=info

# Email/SMTP configuration
# SMTP_HOST=smtp.example.com
# SMTP_PORT=587
# SMTP_USER=notifications@example.com
# SMTP_PASSWORD=your-smtp-password

# External API keys
# OPENAI_API_KEY=sk-...
# STRIPE_PUBLISHABLE_KEY=pk_live_...
# AWS_ACCESS_KEY_ID=AKIA...
# AWS_SECRET_ACCESS_KEY=...

# ============================================================================
# TIPS
# ============================================================================
# 1. Use meaningful variable names
# 2. Document each variable with a comment
# 3. Use 'tako secrets' for sensitive data (passwords, API keys)
# 4. Keep this .env.example file updated as a template for your team
# 5. Validate variables are set before deploying: tako secrets validate

# ============================================================================
# USEFUL COMMANDS
# ============================================================================
# tako setup          - Provision server (Docker, Traefik, security)
# tako deploy         - Deploy your application
# tako deploy -y      - Deploy without confirmation prompts
# tako ps             - List running services
# tako logs --service web  - View service logs
# tako access         - View HTTP access logs (like Vercel)
# tako scale web=3    - Scale service to 3 replicas
# tako stop --service web  - Stop a service (scale to 0)
# tako start --service web - Start a stopped service
# tako rollback       - Rollback to previous deployment
# tako backup --volume data - Backup a Docker volume
# tako backup --list  - List available backups
# tako drift          - Detect configuration drift
# tako history        - View deployment history
# tako destroy        - Remove all services (keeps infrastructure)
# tako --version      - Show CLI version and build info
`

	if err := os.WriteFile(".env.example", []byte(envExample), 0644); err != nil {
		fmt.Printf("Warning: Failed to create .env.example: %v\n", err)
	}

	// Create .gitignore if it doesn't exist
	gitignorePath := ".gitignore"
	gitignoreContent := `.env
.env.local
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

	absPath, _ := filepath.Abs(configPath)

	fmt.Printf("✓ Created tako.yaml at %s\n", absPath)
	fmt.Printf("✓ Created .env.example\n")
	if _, err := os.Stat(gitignorePath); err == nil {
		fmt.Printf("✓ Created .gitignore\n")
	}
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  1. Edit tako.yaml with your server details\n")
	fmt.Printf("  2. Copy .env.example to .env and fill in your secrets\n")
	fmt.Printf("  3. Run 'tako setup' to provision your server\n")
	fmt.Printf("  4. Run 'tako deploy' to deploy your application\n")
	fmt.Printf("\nUseful commands:\n")
	fmt.Printf("  tako ps              - List running services\n")
	fmt.Printf("  tako logs --service  - View service logs\n")
	fmt.Printf("  tako access          - View HTTP access logs\n")
	fmt.Printf("  tako scale web=3     - Scale to 3 replicas\n")
	fmt.Printf("  tako rollback        - Rollback deployment\n")
	fmt.Printf("  tako --help          - Show all commands\n")

	return nil
}
