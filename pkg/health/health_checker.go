package health

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HealthChecker performs network-level checks for public services.
type HealthChecker struct {
	timeout time.Duration
}

// NewHealthChecker creates a new network health checker.
func NewHealthChecker() *HealthChecker {
	return &HealthChecker{
		timeout: 10 * time.Second,
	}
}

// MonitorSSLProvisioning monitors SSL certificate provisioning with periodic checks.
func (h *HealthChecker) MonitorSSLProvisioning(ctx context.Context, serviceName, domain string, maxWait time.Duration) error {
	fmt.Printf("\n🔐 Monitoring SSL certificate provisioning for %s (%s)...\n", serviceName, domain)
	fmt.Printf("   This may take up to 2 minutes for Let's Encrypt validation\n\n")

	startTime := time.Now()
	deadline := time.NewTimer(maxWait)
	defer deadline.Stop()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	attempt := 1
	check := func() bool {
		fmt.Printf("   [%d] Checking SSL status... ", attempt)
		attempt++

		sslInfo := h.checkSSL(domain)
		if sslInfo != nil && sslInfo.Valid {
			elapsed := time.Since(startTime)
			fmt.Printf("✓ SSL certificate active!\n")
			fmt.Printf("\n✓ Certificate Details:\n")
			fmt.Printf("   Issuer: %s\n", sslInfo.Issuer)
			fmt.Printf("   Expires: %s\n", sslInfo.Expiry.Format("2006-01-02 15:04:05"))
			fmt.Printf("   Provisioned in: %v\n", elapsed.Round(time.Second))
			return true
		}

		if sslInfo != nil && sslInfo.Error != "" {
			fmt.Printf("⏳ Waiting: %s\n", simplifySSLError(sslInfo.Error))
			return false
		}

		fmt.Printf("⏳ Waiting...\n")
		return false
	}

	if check() {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("SSL provisioning timeout after %v", maxWait)
		case <-ticker.C:
			if check() {
				return nil
			}
		}
	}
}

// SSLInfo contains SSL certificate information.
type SSLInfo struct {
	Valid  bool
	Issuer string
	Expiry time.Time
	Error  string
}

// checkSSL checks HTTPS accessibility and SSL certificate validity.
func (h *HealthChecker) checkSSL(domain string) *SSLInfo {
	client := &http.Client{
		Timeout: h.timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: false,
			},
		},
	}

	resp, err := client.Get(fmt.Sprintf("https://%s", domain))
	if err != nil {
		if strings.Contains(err.Error(), "certificate") {
			return &SSLInfo{
				Valid: false,
				Error: err.Error(),
			}
		}
		return nil
	}
	defer resp.Body.Close()

	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return nil
	}

	cert := resp.TLS.PeerCertificates[0]
	return &SSLInfo{
		Valid:  true,
		Issuer: cert.Issuer.CommonName,
		Expiry: cert.NotAfter,
	}
}

func simplifySSLError(message string) string {
	switch {
	case strings.Contains(message, "certificate has expired"):
		return "certificate has expired"
	case strings.Contains(message, "not yet valid"):
		return "certificate is not valid yet"
	case strings.Contains(message, "unknown authority"):
		return "certificate authority is not trusted yet"
	case strings.Contains(message, "not valid for any names"):
		return "certificate name does not match this domain"
	default:
		return "certificate is not ready yet"
	}
}
