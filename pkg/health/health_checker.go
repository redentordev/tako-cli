package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/ssh"
	"golang.org/x/sync/errgroup"
)

// ServiceHealth represents the health status of a deployed service
type ServiceHealth struct {
	ServiceName      string
	Domain           string
	HTTPAccessible   bool
	HTTPSAccessible  bool
	SSLValid         bool
	SSLIssuer        string
	SSLExpiry        time.Time
	ContainerRunning bool
	ProxyConfigured  bool
	LastChecked      time.Time
	Errors           []string
}

// HealthChecker performs periodic health checks on deployed services
type HealthChecker struct {
	client        *ssh.Client
	checkInterval time.Duration
	timeout       time.Duration
}

// NewHealthChecker creates a new health checker
func NewHealthChecker(client *ssh.Client) *HealthChecker {
	return &HealthChecker{
		client:        client,
		checkInterval: 30 * time.Second,
		timeout:       10 * time.Second,
	}
}

// CheckService performs a comprehensive health check on a service
// Checks are run in parallel for improved performance
func (h *HealthChecker) CheckService(ctx context.Context, serviceName, domain string) (*ServiceHealth, error) {
	health := &ServiceHealth{
		ServiceName: serviceName,
		Domain:      domain,
		LastChecked: time.Now(),
		Errors:      []string{},
	}

	var mu sync.Mutex

	// Run checks in parallel using errgroup
	g, ctx := errgroup.WithContext(ctx)

	// Container check (requires SSH)
	g.Go(func() error {
		running := h.checkContainerRunning(serviceName)
		mu.Lock()
		health.ContainerRunning = running
		if !running {
			health.Errors = append(health.Errors, "Container not running")
		}
		mu.Unlock()
		return nil
	})

	// Proxy check (requires SSH)
	g.Go(func() error {
		configured := h.checkProxyConfig(serviceName)
		mu.Lock()
		health.ProxyConfigured = configured
		if !configured {
			health.Errors = append(health.Errors, "Proxy not configured properly")
		}
		mu.Unlock()
		return nil
	})

	// HTTP check (network, can run in parallel)
	g.Go(func() error {
		accessible := h.checkHTTPAccess(domain)
		mu.Lock()
		health.HTTPAccessible = accessible
		if !accessible {
			health.Errors = append(health.Errors, "HTTP not accessible")
		}
		mu.Unlock()
		return nil
	})

	// HTTPS/SSL check (network, can run in parallel)
	g.Go(func() error {
		info := h.checkSSL(domain)
		mu.Lock()
		if info != nil {
			health.HTTPSAccessible = true
			health.SSLValid = info.Valid
			health.SSLIssuer = info.Issuer
			health.SSLExpiry = info.Expiry
			if !info.Valid {
				health.Errors = append(health.Errors, fmt.Sprintf("SSL invalid: %s", info.Error))
			}
		} else {
			health.HTTPSAccessible = false
			health.Errors = append(health.Errors, "HTTPS not accessible")
		}
		mu.Unlock()
		return nil
	})

	// Wait for all checks to complete
	if err := g.Wait(); err != nil {
		return nil, err
	}

	return health, nil
}

// MonitorSSLProvisioning monitors SSL certificate provisioning with periodic checks
func (h *HealthChecker) MonitorSSLProvisioning(ctx context.Context, serviceName, domain string, maxWait time.Duration) error {
	fmt.Printf("\n🔐 Monitoring SSL certificate provisioning for %s...\n", domain)
	fmt.Printf("   This may take up to 2 minutes for Let's Encrypt validation\n\n")

	startTime := time.Now()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	attempt := 1
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(maxWait):
			return fmt.Errorf("SSL provisioning timeout after %v", maxWait)
		case <-ticker.C:
			fmt.Printf("   [%d] Checking SSL status... ", attempt)

			sslInfo := h.checkSSL(domain)
			if sslInfo != nil && sslInfo.Valid {
				elapsed := time.Since(startTime)
				fmt.Printf("✓ SSL certificate active!\n")
				fmt.Printf("\n✓ Certificate Details:\n")
				fmt.Printf("   Issuer: %s\n", sslInfo.Issuer)
				fmt.Printf("   Expires: %s\n", sslInfo.Expiry.Format("2006-01-02 15:04:05"))
				fmt.Printf("   Provisioned in: %v\n", elapsed.Round(time.Second))
				return nil
			}

			// Check proxy logs for errors.
			if attempt%3 == 0 {
				errors := h.getProxySSLErrors(domain)
				if len(errors) > 0 {
					fmt.Printf("⚠️  Issues detected\n")
					for _, err := range errors {
						fmt.Printf("      - %s\n", err)
					}
				} else {
					fmt.Printf("⏳ Still provisioning...\n")
				}
			} else {
				fmt.Printf("⏳ Waiting...\n")
			}

			attempt++
		}
	}
}

// checkContainerRunning checks if the container is running
func (h *HealthChecker) checkContainerRunning(serviceName string) bool {
	cmd := fmt.Sprintf("docker ps --filter label=tako.service=%s --format '{{.Names}}'", serviceName)
	output, err := h.client.Execute(cmd)
	return err == nil && strings.TrimSpace(output) != ""
}

// checkProxyConfig checks if the service has proxy labels.
func (h *HealthChecker) checkProxyConfig(serviceName string) bool {
	cmd := fmt.Sprintf("docker ps --filter label=tako.service=%s --filter label=traefik.enable=true --format '{{.Names}}'", serviceName)
	output, err := h.client.Execute(cmd)
	return err == nil && strings.TrimSpace(output) != ""
}

// checkHTTPAccess checks if the service is accessible via HTTP
func (h *HealthChecker) checkHTTPAccess(domain string) bool {
	client := &http.Client{
		Timeout: h.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	resp, err := client.Get(fmt.Sprintf("http://%s", domain))
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// Accept any response (even 404) as long as we got a response
	return resp.StatusCode > 0
}

// SSLInfo contains SSL certificate information
type SSLInfo struct {
	Valid  bool
	Issuer string
	Expiry time.Time
	Error  string
}

// checkSSL checks HTTPS accessibility and SSL certificate validity
func (h *HealthChecker) checkSSL(domain string) *SSLInfo {
	client := &http.Client{
		Timeout: h.timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false, // Verify SSL
			},
		},
	}

	resp, err := client.Get(fmt.Sprintf("https://%s", domain))
	if err != nil {
		// Try to get more info about the SSL error
		if strings.Contains(err.Error(), "certificate") {
			return &SSLInfo{
				Valid: false,
				Error: err.Error(),
			}
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		return &SSLInfo{
			Valid:  true,
			Issuer: cert.Issuer.CommonName,
			Expiry: cert.NotAfter,
		}
	}

	return nil
}

// getProxySSLErrors retrieves SSL-related errors from proxy logs.
func (h *HealthChecker) getProxySSLErrors(domain string) []string {
	cmd := fmt.Sprintf("docker logs --tail 50 tako-proxy 2>&1 | grep -i 'error.*%s\\|acme.*%s' | tail -5", domain, domain)
	output, err := h.client.Execute(cmd)
	if err != nil || output == "" {
		return nil
	}

	errors := []string{}
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Extract meaningful error messages
		if strings.Contains(line, "Timeout during connect") {
			errors = append(errors, "Let's Encrypt timeout - check firewall allows port 80")
		} else if strings.Contains(line, "urn:ietf:params:acme:error") {
			errors = append(errors, "ACME validation failed - domain must be accessible via HTTP")
		} else if strings.Contains(line, "Unable to obtain ACME certificate") {
			errors = append(errors, "Certificate provisioning failed")
		}
	}

	return errors
}

// WaitForServiceReady waits for a service to be fully ready
func (h *HealthChecker) WaitForServiceReady(ctx context.Context, serviceName, domain string, maxWait time.Duration) error {
	fmt.Printf("\n⏳ Waiting for service to be ready...\n")

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(maxWait):
			return fmt.Errorf("service not ready after %v", maxWait)
		case <-ticker.C:
			health, _ := h.CheckService(ctx, serviceName, domain)

			if health.ContainerRunning && health.HTTPAccessible {
				fmt.Printf("✓ Service is ready!\n")
				return nil
			}

			if !health.ContainerRunning {
				fmt.Printf("   Container starting...\n")
			} else if !health.HTTPAccessible {
				fmt.Printf("   Waiting for HTTP response...\n")
			}
		}
	}
}

// PrintHealthReport prints a formatted health report
func (h *ServiceHealth) PrintReport() {
	fmt.Printf("\n📊 Health Check Report: %s\n", h.ServiceName)
	fmt.Printf("   Domain: %s\n", h.Domain)
	fmt.Printf("   Checked: %s\n\n", h.LastChecked.Format("2006-01-02 15:04:05"))

	// Container status
	if h.ContainerRunning {
		fmt.Printf("   ✓ Container: Running\n")
	} else {
		fmt.Printf("   ✗ Container: Not running\n")
	}

	// Proxy status
	if h.ProxyConfigured {
		fmt.Printf("   ✓ Proxy: Configured\n")
	} else {
		fmt.Printf("   ✗ Proxy: Not configured\n")
	}

	// HTTP status
	if h.HTTPAccessible {
		fmt.Printf("   ✓ HTTP: Accessible\n")
	} else {
		fmt.Printf("   ✗ HTTP: Not accessible\n")
	}

	// HTTPS/SSL status
	if h.HTTPSAccessible {
		if h.SSLValid {
			fmt.Printf("   ✓ HTTPS: Accessible (SSL Valid)\n")
			fmt.Printf("     Issuer: %s\n", h.SSLIssuer)
			fmt.Printf("     Expires: %s\n", h.SSLExpiry.Format("2006-01-02"))
		} else {
			fmt.Printf("   ⚠️  HTTPS: SSL Invalid\n")
		}
	} else {
		fmt.Printf("   ⏳ HTTPS: Not yet provisioned\n")
	}

	// Errors
	if len(h.Errors) > 0 {
		fmt.Printf("\n   ⚠️  Issues:\n")
		for _, err := range h.Errors {
			fmt.Printf("      - %s\n", err)
		}
	}

	fmt.Println()
}
