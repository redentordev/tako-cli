package nginx

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

// Manager handles Nginx proxy configuration for Swarm deployments
type Manager struct {
	client      *ssh.Client
	projectName string
	environment string
	verbose     bool
}

// NewManager creates a new Nginx manager
func NewManager(client *ssh.Client, projectName, environment string, verbose bool) *Manager {
	return &Manager{
		client:      client,
		projectName: projectName,
		environment: environment,
		verbose:     verbose,
	}
}

// EnsureNginxService ensures Nginx is running as a Swarm service
func (m *Manager) EnsureNginxService(networkName string) error {
	// Check if nginx service already exists
	checkCmd := "docker service ls --filter name=nginx-proxy --format '{{.Name}}'"
	output, _ := m.client.Execute(checkCmd)

	if strings.TrimSpace(output) == "nginx-proxy" {
		if m.verbose {
			fmt.Println("  Nginx proxy service already exists")
		}
		return nil
	}

	if m.verbose {
		fmt.Println("  Creating Nginx proxy service...")
	}

	// Create directories for nginx config and SSL certificates
	dirs := []string{
		"/etc/nginx/conf.d",
		"/etc/nginx/ssl",
		"/etc/letsencrypt",
		"/var/log/nginx",
	}

	for _, dir := range dirs {
		cmd := fmt.Sprintf("sudo mkdir -p %s", dir)
		if _, err := m.client.Execute(cmd); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	// Create main nginx.conf with proper resolver for Swarm DNS
	nginxConf := `user nginx;
worker_processes auto;
error_log /var/log/nginx/error.log warn;
pid /var/run/nginx.pid;

events {
    worker_connections 1024;
}

http {
    include /etc/nginx/mime.types;
    default_type application/octet-stream;

    # Logging
    log_format main '$remote_addr - $remote_user [$time_local] "$request" '
                    '$status $body_bytes_sent "$http_referer" '
                    '"$http_user_agent" "$http_x_forwarded_for"';

    access_log /var/log/nginx/access.log main;

    # Performance
    sendfile on;
    tcp_nopush on;
    tcp_nodelay on;
    keepalive_timeout 65;
    types_hash_max_size 2048;
    client_max_body_size 100M;

    # Gzip
    gzip on;
    gzip_vary on;
    gzip_min_length 1024;
    gzip_types text/plain text/css text/xml text/javascript
               application/json application/javascript application/xml+rss;

    # DNS resolver for Swarm service discovery
    # 127.0.0.11 is Docker's embedded DNS server
    resolver 127.0.0.11 valid=10s ipv6=off;
    resolver_timeout 5s;

    # Include all server configurations
    include /etc/nginx/conf.d/*.conf;
}`

	// Write nginx.conf
	writeCmd := fmt.Sprintf("echo '%s' | sudo tee /etc/nginx/nginx.conf > /dev/null",
		strings.ReplaceAll(nginxConf, "'", "'\\''"))
	if _, err := m.client.Execute(writeCmd); err != nil {
		return fmt.Errorf("failed to write nginx.conf: %w", err)
	}

	// Deploy Nginx as a Swarm service
	createCmd := fmt.Sprintf(`docker service create --detach \
		--name nginx-proxy \
		--network %s \
		--publish published=80,target=80,mode=ingress \
		--publish published=443,target=443,mode=ingress \
		--mount type=bind,source=/etc/nginx/nginx.conf,target=/etc/nginx/nginx.conf,readonly \
		--mount type=bind,source=/etc/nginx/conf.d,target=/etc/nginx/conf.d,readonly \
		--mount type=bind,source=/etc/nginx/ssl,target=/etc/nginx/ssl,readonly \
		--mount type=bind,source=/etc/letsencrypt,target=/etc/letsencrypt \
		--mount type=bind,source=/var/log/nginx,target=/var/log/nginx \
		--constraint node.role==manager \
		--replicas 1 \
		--update-parallelism 1 \
		--update-delay 10s \
		nginx:alpine 2>&1`, networkName)

	if _, err := m.client.Execute(createCmd); err != nil {
		return fmt.Errorf("failed to create nginx service: %w", err)
	}

	// Wait for service to be ready
	time.Sleep(5 * time.Second)

	if m.verbose {
		fmt.Println("  ✓ Nginx proxy service created")
	}

	return nil
}

// UpdateServiceConfig updates Nginx configuration for a specific service
func (m *Manager) UpdateServiceConfig(serviceName string, service *config.ServiceConfig, fullServiceName string) error {
	if m.verbose {
		fmt.Printf("  Updating Nginx config for service %s...\n", serviceName)
	}

	// Generate nginx server block
	nginxConfig := m.generateServiceConfig(serviceName, service, fullServiceName)

	// Write to service-specific config file
	configFile := fmt.Sprintf("/etc/nginx/conf.d/%s_%s.conf", m.projectName, serviceName)

	// Escape single quotes for shell
	nginxConfig = strings.ReplaceAll(nginxConfig, "'", "'\\''")

	writeCmd := fmt.Sprintf("echo '%s' | sudo tee %s > /dev/null", nginxConfig, configFile)
	if _, err := m.client.Execute(writeCmd); err != nil {
		return fmt.Errorf("failed to write nginx config: %w", err)
	}

	// Skip nginx config test for now - will be validated when service loads the config
	// The nginx service itself will validate the config on startup/reload
	if m.verbose {
		fmt.Println("  Config written, nginx will validate on reload")
	}

	// Reload nginx service
	reloadCmd := "docker service update --force nginx-proxy"
	if _, err := m.client.Execute(reloadCmd); err != nil {
		return fmt.Errorf("failed to reload nginx: %w", err)
	}

	if m.verbose {
		fmt.Printf("  ✓ Nginx config updated for %s\n", serviceName)
	}

	return nil
}

// generateServiceConfig generates Nginx configuration for a service
func (m *Manager) generateServiceConfig(serviceName string, service *config.ServiceConfig, fullServiceName string) string {
	var config strings.Builder

	// Create upstream block using Swarm service name
	config.WriteString(fmt.Sprintf("# Configuration for %s\n", serviceName))
	config.WriteString(fmt.Sprintf("upstream %s_backend {\n", serviceName))
	config.WriteString("    # Use Swarm DNS to resolve and load balance\n")
	config.WriteString(fmt.Sprintf("    server %s:%d;\n", fullServiceName, service.Port))
	config.WriteString("    keepalive 32;\n")
	config.WriteString("}\n\n")

	// For each domain, create a server block
	for _, domain := range service.Proxy.Domains {
		// HTTP server block (redirects to HTTPS or serves directly)
		config.WriteString("server {\n")
		config.WriteString("    listen 80;\n")
		config.WriteString(fmt.Sprintf("    server_name %s;\n\n", domain))

		// If we have SSL setup, redirect to HTTPS
		if service.Proxy.Email != "" {
			config.WriteString("    # Redirect to HTTPS\n")
			config.WriteString("    location / {\n")
			config.WriteString(fmt.Sprintf("        return 301 https://%s$request_uri;\n", domain))
			config.WriteString("    }\n\n")

			// Let's Encrypt challenge location
			config.WriteString("    # Let's Encrypt challenge\n")
			config.WriteString("    location /.well-known/acme-challenge/ {\n")
			config.WriteString("        root /var/www/certbot;\n")
			config.WriteString("    }\n")
		} else {
			// No SSL, proxy directly
			config.WriteString(m.generateProxyBlock(serviceName, service))
		}

		config.WriteString("}\n\n")

		// HTTPS server block (if SSL is configured)
		if service.Proxy.Email != "" {
			config.WriteString("server {\n")
			config.WriteString("    listen 443 ssl http2;\n")
			config.WriteString(fmt.Sprintf("    server_name %s;\n\n", domain))

			// SSL certificates (we'll use certbot to generate these)
			sslPath := fmt.Sprintf("/etc/letsencrypt/live/%s", domain)
			config.WriteString("    # SSL Configuration\n")
			config.WriteString(fmt.Sprintf("    ssl_certificate %s/fullchain.pem;\n", sslPath))
			config.WriteString(fmt.Sprintf("    ssl_certificate_key %s/privkey.pem;\n\n", sslPath))

			// SSL security settings
			config.WriteString("    # SSL Security\n")
			config.WriteString("    ssl_protocols TLSv1.2 TLSv1.3;\n")
			config.WriteString("    ssl_ciphers HIGH:!aNULL:!MD5;\n")
			config.WriteString("    ssl_prefer_server_ciphers on;\n")
			config.WriteString("    ssl_session_cache shared:SSL:10m;\n")
			config.WriteString("    ssl_session_timeout 10m;\n\n")

			// Security headers
			config.WriteString("    # Security Headers\n")
			config.WriteString("    add_header X-Frame-Options \"SAMEORIGIN\" always;\n")
			config.WriteString("    add_header X-Content-Type-Options \"nosniff\" always;\n")
			config.WriteString("    add_header X-XSS-Protection \"1; mode=block\" always;\n\n")

			// Proxy configuration
			config.WriteString(m.generateProxyBlock(serviceName, service))

			config.WriteString("}\n\n")
		}
	}

	return config.String()
}

// generateProxyBlock generates the location block for proxying
func (m *Manager) generateProxyBlock(serviceName string, service *config.ServiceConfig) string {
	var block strings.Builder

	block.WriteString("    # Proxy to backend service\n")
	block.WriteString("    location / {\n")
	block.WriteString(fmt.Sprintf("        proxy_pass http://%s_backend;\n", serviceName))
	block.WriteString("        proxy_http_version 1.1;\n\n")

	// Headers
	block.WriteString("        # Headers\n")
	block.WriteString("        proxy_set_header Host $host;\n")
	block.WriteString("        proxy_set_header X-Real-IP $remote_addr;\n")
	block.WriteString("        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	block.WriteString("        proxy_set_header X-Forwarded-Proto $scheme;\n")
	block.WriteString("        proxy_set_header X-Forwarded-Host $host;\n")
	block.WriteString("        proxy_set_header X-Forwarded-Port $server_port;\n\n")

	// WebSocket support
	block.WriteString("        # WebSocket support\n")
	block.WriteString("        proxy_set_header Upgrade $http_upgrade;\n")
	block.WriteString("        proxy_set_header Connection \"upgrade\";\n\n")

	// Timeouts
	block.WriteString("        # Timeouts\n")
	block.WriteString("        proxy_connect_timeout 60s;\n")
	block.WriteString("        proxy_send_timeout 60s;\n")
	block.WriteString("        proxy_read_timeout 60s;\n\n")

	// Buffering
	block.WriteString("        # Buffering\n")
	block.WriteString("        proxy_buffering off;\n")
	block.WriteString("        proxy_request_buffering off;\n")

	block.WriteString("    }\n\n")

	// Health check endpoint (if configured)
	if service.HealthCheck.Path != "" {
		block.WriteString("    # Health check endpoint\n")
		block.WriteString(fmt.Sprintf("    location %s {\n", service.HealthCheck.Path))
		block.WriteString(fmt.Sprintf("        proxy_pass http://%s_backend;\n", serviceName))
		block.WriteString("        access_log off;\n")
		block.WriteString("    }\n")
	}

	return block.String()
}

// SetupSSL sets up SSL certificates using Let's Encrypt
func (m *Manager) SetupSSL(domain, email string) error {
	if m.verbose {
		fmt.Printf("  Setting up SSL for %s...\n", domain)
	}

	// Install certbot if not present
	installCmd := "which certbot || sudo apt-get update && sudo apt-get install -y certbot"
	if _, err := m.client.Execute(installCmd); err != nil {
		return fmt.Errorf("failed to install certbot: %w", err)
	}

	// Create webroot directory
	m.client.Execute("sudo mkdir -p /var/www/certbot")

	// Get certificate (using webroot method)
	certCmd := fmt.Sprintf(
		"sudo certbot certonly --webroot -w /var/www/certbot --email %s --agree-tos --non-interactive -d %s",
		email, domain)

	if output, err := m.client.Execute(certCmd); err != nil {
		// Check if certificate already exists
		if strings.Contains(output, "Certificate not yet due for renewal") {
			if m.verbose {
				fmt.Printf("  Certificate already exists for %s\n", domain)
			}
			return nil
		}
		return fmt.Errorf("failed to obtain certificate: %s", output)
	}

	// Setup auto-renewal via cron
	cronCmd := `(crontab -l 2>/dev/null | grep -v certbot; echo "0 0,12 * * * /usr/bin/certbot renew --quiet --post-hook 'docker service update --force nginx-proxy'") | crontab -`
	m.client.Execute(cronCmd)

	if m.verbose {
		fmt.Printf("  ✓ SSL certificate obtained for %s\n", domain)
	}

	return nil
}

// RemoveServiceConfig removes Nginx configuration for a service
func (m *Manager) RemoveServiceConfig(serviceName string) error {
	configFile := fmt.Sprintf("/etc/nginx/conf.d/%s_%s.conf", m.projectName, serviceName)

	removeCmd := fmt.Sprintf("sudo rm -f %s", configFile)
	if _, err := m.client.Execute(removeCmd); err != nil {
		return fmt.Errorf("failed to remove nginx config: %w", err)
	}

	// Reload nginx
	m.client.Execute("docker service update --force nginx-proxy")

	return nil
}
