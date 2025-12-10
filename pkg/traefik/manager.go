package traefik

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Manager handles Traefik proxy configuration for Swarm deployments
type Manager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// NewManager creates a new Traefik manager
func NewManager(client *ssh.Client, projectName, environment string, verbose bool) *Manager {
	return &Manager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// EnsureTraefikService ensures Traefik is running as a Swarm service
func (m *Manager) EnsureTraefikService(networkName string) error {
	// Check if traefik service already exists
	checkCmd := "docker service ls --filter name=traefik --format '{{.Name}}'"
	output, _ := m.client.Execute(checkCmd)

	if strings.TrimSpace(output) == "traefik" {
		if m.verbose {
			fmt.Println("  Traefik proxy service already exists")
		}
		// Ensure it's on our network
		m.client.Execute(fmt.Sprintf("docker service update --network-add %s traefik 2>/dev/null", networkName))
		return nil
	}

	// MIGRATION: Check if standalone Traefik container exists and remove it
	// This happens when transitioning from single-server to swarm mode
	standaloneCheck := "docker ps -a --filter name=^traefik$ --format '{{.Names}}'"
	standaloneOutput, _ := m.client.Execute(standaloneCheck)
	if strings.TrimSpace(standaloneOutput) == "traefik" {
		if m.verbose {
			fmt.Println("  Migrating standalone Traefik container to Swarm service...")
			fmt.Println("  Stopping and removing standalone Traefik container...")
		}
		// Stop and remove standalone container
		m.client.Execute("docker stop traefik 2>/dev/null")
		m.client.Execute("docker rm traefik 2>/dev/null")
		if m.verbose {
			fmt.Println("  ✓ Standalone Traefik container removed")
		}
	}

	if m.verbose {
		fmt.Println("  Creating Traefik proxy service...")
	}

	// Create directories for Traefik
	dirs := []string{
		"/etc/traefik",
		"/etc/traefik/acme",
		"/var/log/traefik",
	}

	for _, dir := range dirs {
		cmd := fmt.Sprintf("sudo mkdir -p %s && sudo chmod 755 %s", dir, dir)
		if _, err := m.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Create Traefik static configuration
	traefikConfig := `
# Traefik static configuration
api:
  dashboard: true
  debug: true

entryPoints:
  web:
    address: ":80"
    http:
      redirections:
        entryPoint:
          to: websecure
          scheme: https
          permanent: true

  websecure:
    address: ":443"
    http:
      tls:
        certResolver: letsencrypt

providers:
  docker:
    swarmMode: true
    exposedByDefault: false
    network: ` + networkName + `
    endpoint: "unix:///var/run/docker.sock"
    watch: true

certificatesResolvers:
  letsencrypt:
    acme:
      email: tako@redentor.dev
      storage: /acme/acme.json
      httpChallenge:
        entryPoint: web
      caServer: https://acme-v02.api.letsencrypt.org/directory

log:
  level: INFO
  filePath: /var/log/traefik/traefik.log

accessLog:
  filePath: /var/log/traefik/access.log
`

	// Write traefik.yml config
	writeCmd := fmt.Sprintf("echo '%s' | sudo tee /etc/traefik/traefik.yml > /dev/null",
		strings.ReplaceAll(traefikConfig, "'", "'\\''"))
	if _, err := m.client.Execute(writeCmd); err != nil {
		return fmt.Errorf("failed to write traefik config: %w", err)
	}

	// Create acme.json with proper permissions
	m.client.Execute("sudo touch /etc/traefik/acme/acme.json && sudo chmod 600 /etc/traefik/acme/acme.json")

	// Deploy Traefik as a Swarm service with BOTH Docker and Swarm providers
	// This allows Traefik to discover both standalone containers AND swarm services
	// Using latest tag to get the most recent Docker client libraries
	// Use mode=host for Traefik ports to preserve real client IPs
	// This works because Traefik is constrained to manager node
	// Backend services still use ingress mode for load balancing across swarm nodes
	// Don't specify a specific network for swarm provider - let Traefik discover services on all networks it's connected to
	// CRITICAL: --restart-condition any ensures Traefik auto-recovers after server reboot
	// NOTE: Dashboard/API is disabled for security - no port 8080 exposure
	createCmd := fmt.Sprintf("docker service create --detach --name traefik --network %s --constraint node.role==manager --restart-condition any --publish published=80,target=80,mode=host --publish published=443,target=443,mode=host --mount type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock,readonly --mount type=bind,source=/etc/traefik/acme,target=/acme --mount type=bind,source=/var/log/traefik,target=/var/log/traefik --replicas 1 traefik:v3.6.1 --api.dashboard=false --providers.docker=true --providers.docker.exposedByDefault=false --providers.docker.endpoint=unix:///var/run/docker.sock --providers.docker.watch=true --providers.swarm=true --providers.swarm.exposedByDefault=false --providers.swarm.endpoint=unix:///var/run/docker.sock --providers.swarm.watch=true --entryPoints.web.address=:80 --entryPoints.web.forwardedHeaders.insecure=true --entryPoints.websecure.address=:443 --entryPoints.websecure.forwardedHeaders.insecure=true --certificatesResolvers.letsencrypt.acme.email=tako@redentor.dev --certificatesResolvers.letsencrypt.acme.storage=/acme/acme.json --certificatesResolvers.letsencrypt.acme.httpChallenge.entryPoint=web --log.level=INFO --accessLog.filePath=/var/log/traefik/access.log --accessLog.format=json 2>&1", networkName)

	if _, err := m.client.Execute(createCmd); err != nil {
		return fmt.Errorf("failed to create traefik service: %w", err)
	}

	// Wait for service to be ready
	time.Sleep(5 * time.Second)

	// Connect Traefik to all existing Tako networks so it can discover standalone containers
	if m.verbose {
		fmt.Println("  Connecting Traefik to existing project networks...")
	}

	// Get all Tako networks
	networksCmd := "docker network ls --filter name=tako_ --format '{{.Name}}'"
	networksOutput, err := m.client.Execute(networksCmd)
	if err == nil {
		networks := strings.Split(strings.TrimSpace(networksOutput), "\n")
		for _, network := range networks {
			if network != "" && network != networkName {
				// Connect Traefik service to this network
				connectCmd := fmt.Sprintf("docker service update --network-add %s traefik 2>/dev/null", network)
				m.client.Execute(connectCmd)
				if m.verbose {
					fmt.Printf("    Connected to network: %s\n", network)
				}
			}
		}
	}

	if m.verbose {
		fmt.Println("  ✓ Traefik proxy service created")
	}

	return nil
}

// GetServiceLabels returns Traefik labels for service creation
// This avoids service updates which can cause brief Traefik config reload disruptions
func (m *Manager) GetServiceLabels(serviceName string, service *config.ServiceConfig, fullServiceName string) []string {
	var labels []string

	// Enable Traefik for this service
	labels = append(labels, "--label \"traefik.enable=true\"")

	// Get all domains using the new helper
	allDomains := service.Proxy.GetAllDomains()
	primaryDomain := service.Proxy.GetPrimaryDomain()

	// Configure routers and services for each domain
	for i, domain := range allDomains {
		// Use fullServiceName to ensure router names are unique across projects
		routerName := fmt.Sprintf("%s-%d", fullServiceName, i)

		// Skip empty domains
		if domain == "" {
			continue
		}

		// HTTPS router with TLS
		labels = append(labels, fmt.Sprintf("--label 'traefik.http.routers.%s.rule=Host(\"%s\")'", routerName, domain))
		labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.entrypoints=websecure\"", routerName))
		labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.service=%s@swarm\"", routerName, fullServiceName))

		// TLS configuration
		if service.Proxy.Email != "" {
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.tls=true\"", routerName))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.tls.certresolver=letsencrypt\"", routerName))

			// HTTP to HTTPS redirect
			httpRouterName := fmt.Sprintf("%s-http", routerName)
			labels = append(labels, fmt.Sprintf("--label 'traefik.http.routers.%s.rule=Host(\"%s\")'", httpRouterName, domain))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.entrypoints=web\"", httpRouterName))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.middlewares=redirect-to-https@swarm\"", httpRouterName))

			// Create redirect middleware (only once per service)
			labels = append(labels, "--label \"traefik.http.middlewares.redirect-to-https.redirectscheme.scheme=https\"")
			labels = append(labels, "--label \"traefik.http.middlewares.redirect-to-https.redirectscheme.permanent=true\"")
		}
	}

	// Add redirect domain labels if configured (e.g., www -> non-www)
	if service.Proxy.HasRedirects() && primaryDomain != "" {
		for i, redirectDomain := range service.Proxy.GetRedirectDomains() {
			redirectRouterName := fmt.Sprintf("%s-redirect-%d", fullServiceName, i)
			middlewareName := fmt.Sprintf("%s-redirect-%d", fullServiceName, i)

			// Escape dots in domain for regex
			escapedRedirectDomain := strings.ReplaceAll(redirectDomain, ".", "\\.")

			// Router for the redirect domain (HTTPS)
			labels = append(labels, fmt.Sprintf("--label 'traefik.http.routers.%s.rule=Host(\"%s\")'", redirectRouterName, redirectDomain))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.entrypoints=websecure\"", redirectRouterName))

			// TLS for redirect domain
			if service.Proxy.Email != "" {
				labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.tls=true\"", redirectRouterName))
				labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.tls.certresolver=letsencrypt\"", redirectRouterName))
			}

			// Redirect middleware
			// Note: Use single quotes to prevent shell expansion of $1
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.middlewares.%s.redirectregex.regex=^https?://%s/(.*)\"", middlewareName, escapedRedirectDomain))
			labels = append(labels, fmt.Sprintf(`--label 'traefik.http.middlewares.%s.redirectregex.replacement=https://%s/$1'`, middlewareName, primaryDomain))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.middlewares.%s.redirectregex.permanent=true\"", middlewareName))

			// Apply middleware to router
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.middlewares=%s\"", redirectRouterName, middlewareName))

			// HTTP router for redirect domain (also needs redirect)
			httpRedirectRouterName := fmt.Sprintf("%s-redirect-%d-http", fullServiceName, i)
			labels = append(labels, fmt.Sprintf("--label 'traefik.http.routers.%s.rule=Host(\"%s\")'", httpRedirectRouterName, redirectDomain))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.entrypoints=web\"", httpRedirectRouterName))
			labels = append(labels, fmt.Sprintf("--label \"traefik.http.routers.%s.middlewares=%s\"", httpRedirectRouterName, middlewareName))
		}

		if m.verbose {
			fmt.Printf("    Configured %d redirect domain(s) -> %s\n", len(service.Proxy.GetRedirectDomains()), primaryDomain)
		}
	}

	// Service configuration - tell Traefik which port to use
	labels = append(labels, fmt.Sprintf("--label \"traefik.http.services.%s.loadbalancer.server.port=%d\"", fullServiceName, service.Port))

	// Health check if configured
	if service.HealthCheck.Path != "" {
		labels = append(labels, fmt.Sprintf("--label \"traefik.http.services.%s.loadbalancer.healthcheck.path=%s\"", fullServiceName, service.HealthCheck.Path))
		labels = append(labels, fmt.Sprintf("--label \"traefik.http.services.%s.loadbalancer.healthcheck.interval=10s\"", fullServiceName))
	}

	return labels
}

// UpdateServiceLabels adds Traefik labels to a service for automatic discovery
// DEPRECATED: Use GetServiceLabels during service creation to avoid disruptions
func (m *Manager) UpdateServiceLabels(serviceName string, service *config.ServiceConfig, fullServiceName string) error {
	if m.verbose {
		fmt.Printf("  Configuring Traefik labels for service %s...\n", serviceName)
	}

	// Build label arguments for docker service update
	var labels []string

	// Enable Traefik for this service
	labels = append(labels, "--label-add \"traefik.enable=true\"")

	// Configure routers and services for each domain
	if m.verbose {
		fmt.Printf("    Number of domains: %d\n", len(service.Proxy.Domains))
	}
	for i, domain := range service.Proxy.Domains {
		// Use fullServiceName to ensure router names are unique across projects
		routerName := fmt.Sprintf("%s-%d", fullServiceName, i)

		// Debug: log the domain value
		if m.verbose {
			fmt.Printf("    Adding router %s for domain: '%s' (len=%d)\n", routerName, domain, len(domain))
		}

		// Skip empty domains
		if domain == "" {
			if m.verbose {
				fmt.Printf("    WARNING: Empty domain at index %d, skipping\n", i)
			}
			continue
		}

		// HTTPS router with TLS
		// Traefik supports both backticks and double quotes in matchers
		// Using escaped double quotes to avoid shell interpretation issues
		labels = append(labels, fmt.Sprintf("--label-add 'traefik.http.routers.%s.rule=Host(\"%s\")'", routerName, domain))
		labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.routers.%s.entrypoints=websecure\"", routerName))
		// For swarm mode, use the full service name for the service reference
		labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.routers.%s.service=%s@swarm\"", routerName, fullServiceName))

		// TLS configuration
		if service.Proxy.Email != "" {
			labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.routers.%s.tls=true\"", routerName))
			labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.routers.%s.tls.certresolver=letsencrypt\"", routerName))

			// HTTP to HTTPS redirect
			httpRouterName := fmt.Sprintf("%s-http", routerName)
			labels = append(labels, fmt.Sprintf("--label-add 'traefik.http.routers.%s.rule=Host(\"%s\")'", httpRouterName, domain))
			labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.routers.%s.entrypoints=web\"", httpRouterName))
			labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.routers.%s.middlewares=redirect-to-https@swarm\"", httpRouterName))

			// Create redirect middleware (only once)
			labels = append(labels, "--label-add \"traefik.http.middlewares.redirect-to-https.redirectscheme.scheme=https\"")
			labels = append(labels, "--label-add \"traefik.http.middlewares.redirect-to-https.redirectscheme.permanent=true\"")

			// Update Traefik config with correct email
			m.updateTraefikEmail(service.Proxy.Email)
		}
	}

	// Service configuration - tell Traefik which port to use
	// For swarm mode, use the full service name but without @swarm in the service definition
	labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.services.%s.loadbalancer.server.port=%d\"", fullServiceName, service.Port))

	// Health check if configured
	if service.HealthCheck.Path != "" {
		labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.services.%s.loadbalancer.healthcheck.path=%s\"", fullServiceName, service.HealthCheck.Path))
		labels = append(labels, fmt.Sprintf("--label-add \"traefik.http.services.%s.loadbalancer.healthcheck.interval=10s\"", fullServiceName))
	}

	// Sticky sessions if needed (optional, for stateful apps)
	// labels = append(labels, fmt.Sprintf("--label-add traefik.http.services.%s.loadbalancer.sticky.cookie=true", serviceName))

	// Update the service with all labels (use --detach to avoid hanging)
	updateCmd := fmt.Sprintf("docker service update --detach %s %s", strings.Join(labels, " "), fullServiceName)

	if m.verbose {
		fmt.Printf("  Updating service with Traefik labels...\n")
	}

	if output, err := m.client.Execute(updateCmd); err != nil {
		return fmt.Errorf("failed to update service labels: %w, output: %s", err, output)
	}

	// Wait for the update to complete with proper timeout
	// Docker service update with --detach returns immediately, but we need to verify it succeeded
	timeout := time.After(15 * time.Second)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			// Timeout reached - log warning but don't fail
			// The update might still succeed, we just can't wait any longer
			if m.verbose {
				fmt.Printf("  ⚠ Timeout waiting for service update to complete, continuing...\n")
			}
			return nil

		case <-ticker.C:
			checkCmd := fmt.Sprintf("docker service inspect %s --format '{{.UpdateStatus.State}}' 2>&1", fullServiceName)
			status, err := m.client.Execute(checkCmd)
			if err != nil {
				// Service might not exist yet, continue waiting
				continue
			}

			status = strings.TrimSpace(status)

			// Empty status, "<no value>", or "completed" means update is done/not needed
			if status == "" || status == "<no value>" || status == "completed" {
				if m.verbose {
					fmt.Printf("  ✓ Traefik labels configured for %s\n", serviceName)
				}
				return nil
			}

			// Check for failure states
			if status == "paused" || status == "rollback_completed" {
				return fmt.Errorf("service update failed with status: %s", status)
			}

			// "updating" is normal, continue waiting
		}
	}
}

// updateTraefikEmail updates the email in Traefik config for Let's Encrypt
func (m *Manager) updateTraefikEmail(email string) {
	// Update the email in the Traefik configuration
	updateCmd := fmt.Sprintf(
		"sudo sed -i 's/email: tako@redentor.dev/email: %s/' /etc/traefik/traefik.yml",
		email)
	m.client.Execute(updateCmd)

	// Reload Traefik to pick up the new email
	m.client.Execute("docker service update --force traefik 2>/dev/null")
}

// RemoveServiceLabels removes Traefik labels from a service
func (m *Manager) RemoveServiceLabels(fullServiceName string) error {
	// Get all traefik labels
	getLabelsCmd := fmt.Sprintf("docker service inspect %s --format '{{range $k, $v := .Spec.Labels}}{{if hasPrefix $k \"traefik\"}}{{$k}} {{end}}{{end}}'", fullServiceName)
	labels, _ := m.client.Execute(getLabelsCmd)

	if labels != "" {
		// Build remove command
		var removeLabels []string
		for _, label := range strings.Fields(labels) {
			if strings.HasPrefix(label, "traefik") {
				removeLabels = append(removeLabels, fmt.Sprintf("--label-rm %s", label))
			}
		}

		if len(removeLabels) > 0 {
			removeCmd := fmt.Sprintf("docker service update %s %s", strings.Join(removeLabels, " "), fullServiceName)
			m.client.Execute(removeCmd)
		}
	}

	return nil
}

// NOTE: Container-mode Traefik functions (EnsureTraefikContainer, GetContainerLabels, etc.)
// have been removed as Tako now always uses Swarm mode with EnsureTraefikService.
