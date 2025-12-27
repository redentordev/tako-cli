package ssl

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// DNSChecker handles DNS propagation verification
type DNSChecker struct {
	resolvers []string
}

// NewDNSChecker creates a new DNS checker with default resolvers
func NewDNSChecker() *DNSChecker {
	return &DNSChecker{
		resolvers: []string{
			"1.1.1.1:53",  // Cloudflare
			"8.8.8.8:53",  // Google
			"9.9.9.9:53",  // Quad9
		},
	}
}

// CheckCNAME checks if a CNAME record exists and points to the expected target
func (c *DNSChecker) CheckCNAME(domain string, expectedTarget string) (bool, error) {
	acmeDomain := "_acme-challenge." + domain

	// Try multiple resolvers
	for _, resolver := range c.resolvers {
		cname, err := c.lookupCNAME(acmeDomain, resolver)
		if err != nil {
			continue // Try next resolver
		}

		// Normalize both values (remove trailing dot)
		cname = strings.TrimSuffix(cname, ".")
		expectedTarget = strings.TrimSuffix(expectedTarget, ".")

		if cname == expectedTarget {
			return true, nil
		}
	}

	return false, nil
}

// lookupCNAME performs a CNAME lookup using a specific resolver
func (c *DNSChecker) lookupCNAME(domain, resolver string) (string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: 5 * time.Second,
			}
			return d.DialContext(ctx, "udp", resolver)
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cname, err := r.LookupCNAME(ctx, domain)
	if err != nil {
		return "", err
	}

	return cname, nil
}

// WaitForDNSPropagation waits for DNS propagation with progress updates
func (c *DNSChecker) WaitForDNSPropagation(ctx context.Context, domain, expectedTarget string, checkInterval time.Duration, onProgress func(attempt int, elapsed time.Duration)) (bool, error) {
	startTime := time.Now()
	attempt := 0

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		default:
			attempt++
			elapsed := time.Since(startTime)

			if onProgress != nil {
				onProgress(attempt, elapsed)
			}

			verified, err := c.CheckCNAME(domain, expectedTarget)
			if err != nil {
				// Log error but continue trying
			}

			if verified {
				return true, nil
			}

			// Wait before next check
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(checkInterval):
			}
		}
	}
}

// CheckPort53Accessible checks if port 53 is accessible on the given IP.
// Note: This only verifies TCP connectivity. UDP "connectivity" cannot be reliably
// verified without sending actual DNS queries, as UDP is connectionless.
// A successful TCP check is a good indicator that the DNS server is running.
func CheckPort53Accessible(serverIP string) error {
	// Try TCP connection to port 53
	// TCP is more reliable for connectivity testing since it's connection-oriented
	conn, err := net.DialTimeout("tcp", serverIP+":53", 5*time.Second)
	if err != nil {
		return fmt.Errorf("port 53 (TCP) is not accessible: %w", err)
	}
	conn.Close()

	// Note: We don't test UDP because net.DialTimeout("udp", ...) will almost
	// always succeed since UDP is connectionless - it doesn't actually verify
	// that anything is listening. The TCP check is sufficient to verify the
	// DNS server is running.

	return nil
}

// DNSPropagationResult represents the result of a DNS propagation check
type DNSPropagationResult struct {
	Domain     string
	Verified   bool
	Attempts   int
	Duration   time.Duration
	Error      error
}
