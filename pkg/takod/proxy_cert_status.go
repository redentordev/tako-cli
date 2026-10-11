package takod

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
)

// CapabilityProxyCertStatusV1 means the node serves GET /v1/certs/status: a
// read-only, key-free report of whether the proxy holds a currently valid
// certificate for one hostname, from the Tako store or Caddy's own storage.
const CapabilityProxyCertStatusV1 = "proxy.cert-status-v1"

// CertificateSourceCaddy marks a certificate Caddy obtained itself (HTTP-01 or
// TLS-ALPN-01) and keeps in its data directory.
const CertificateSourceCaddy = "caddy"

// ProxyCertificateStatusResponse reports the node-local certificate serving
// one hostname. It never contains PEM material or private keys.
type ProxyCertificateStatusResponse struct {
	Domain string `json:"domain"`
	// Present is true when a certificate covering Domain exists on the node.
	Present bool `json:"present"`
	// Valid is true when that certificate covers Domain and is inside its
	// validity window now.
	Valid      bool      `json:"valid"`
	Source     string    `json:"source,omitempty"`
	Name       string    `json:"name,omitempty"`
	Issuer     string    `json:"issuer,omitempty"`
	NotBefore  time.Time `json:"notBefore,omitempty"`
	NotAfter   time.Time `json:"notAfter,omitempty"`
	Error      string    `json:"error,omitempty"`
	CheckedAt  time.Time `json:"checkedAt"`
	StoreError string    `json:"storeError,omitempty"`
}

// ProxyCertificateStatus inspects the node-local certificate for host. The
// Tako certificate store (pushed and DNS-01 certificates, which take routing
// precedence) is consulted first, then Caddy's automatic-HTTPS storage.
func ProxyCertificateStatus(ctx context.Context, host string) (*ProxyCertificateStatusResponse, error) {
	domain, err := normalizeProxyCertificateDomain(host)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(domain, "*.") {
		return nil, fmt.Errorf("certificate status requires a concrete hostname, not %s", domain)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := proxyCertificateNow().UTC()
	response := &ProxyCertificateStatusResponse{Domain: domain, CheckedAt: now}

	proxyCertificateMu.Lock()
	entries, storeErr := loadProxyCertificateEntries(false)
	proxyCertificateMu.Unlock()
	if storeErr != nil {
		response.StoreError = storeErr.Error()
	} else if entry := selectProxyCertificate(entries, domain); entry != nil {
		fillProxyCertificateStatus(response, entry.Leaf, entry.Metadata.Source, entry.Metadata.Domain, now)
		return response, nil
	}

	if leaf, name := latestCaddyCertificate(domain, now); leaf != nil {
		fillProxyCertificateStatus(response, leaf, CertificateSourceCaddy, name, now)
	}
	return response, nil
}

func fillProxyCertificateStatus(response *ProxyCertificateStatusResponse, leaf *x509.Certificate, source, name string, now time.Time) {
	response.Present = true
	response.Source = source
	response.Name = name
	response.Issuer = certificateIssuerName(leaf)
	response.NotBefore = leaf.NotBefore.UTC()
	response.NotAfter = leaf.NotAfter.UTC()
	response.Valid, response.Error = true, ""
	switch {
	case now.Before(leaf.NotBefore):
		response.Valid, response.Error = false, "certificate is not valid until "+leaf.NotBefore.UTC().Format(time.RFC3339)
	case !now.Before(leaf.NotAfter):
		response.Valid, response.Error = false, "certificate expired at "+leaf.NotAfter.UTC().Format(time.RFC3339)
	default:
		if err := leaf.VerifyHostname(response.Domain); err != nil {
			response.Valid, response.Error = false, "certificate does not cover "+response.Domain
		}
	}
}

func certificateIssuerName(leaf *x509.Certificate) string {
	if leaf.Issuer.CommonName != "" {
		return leaf.Issuer.CommonName
	}
	if len(leaf.Issuer.Organization) > 0 {
		return leaf.Issuer.Organization[0]
	}
	return ""
}

// latestCaddyCertificate returns the best certificate Caddy stored for host,
// exact names before a covering wildcard, preferring currently valid ones
// and then the latest expiry. Caddy keeps one directory per ACME issuer under
// <data>/caddy/certificates/<issuer>/<name>/<name>.crt.
func latestCaddyCertificate(host string, now time.Time) (*x509.Certificate, string) {
	names := []string{host}
	if index := strings.Index(host, "."); index > 0 && strings.Contains(host[index+1:], ".") {
		names = append(names, "*"+host[index:])
	}
	for _, name := range names {
		safe := certmagic.StorageKeys.Safe(name)
		paths, err := filepath.Glob(filepath.Join(proxyCaddyDataDir, "caddy", "certificates", "*", safe, safe+".crt"))
		if err != nil {
			continue
		}
		var best *x509.Certificate
		bestValid := false
		for _, path := range paths {
			leaf := readCaddyLeafCertificate(path)
			if leaf == nil {
				continue
			}
			valid := !now.Before(leaf.NotBefore) && now.Before(leaf.NotAfter) && leaf.VerifyHostname(host) == nil
			if best == nil || (valid && !bestValid) || (valid == bestValid && leaf.NotAfter.After(best.NotAfter)) {
				best, bestValid = leaf, valid
			}
		}
		if best != nil {
			return best, name
		}
	}
	return nil, ""
}

func readCaddyLeafCertificate(path string) *x509.Certificate {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return nil
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		leaf, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil
		}
		return leaf
	}
}

func (s *Server) handleProxyCertificateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	response, err := ProxyCertificateStatus(r.Context(), r.URL.Query().Get("domain"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(response)
}
