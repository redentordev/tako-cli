package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// NodeCertificateEvidence is what the proxy nodes report about one domain's
// node-local certificate.
type NodeCertificateEvidence struct {
	// Supported is false when any proxy node could not be asked (older takod
	// without proxy.cert-status-v1, or unreachable). Callers then keep the
	// public-probe result unchanged.
	Supported bool
	// Valid is true only when every proxy node holds a currently valid
	// certificate covering the domain.
	Valid    bool
	Servers  []string
	Source   string
	Issuer   string
	NotAfter time.Time
	Detail   string
}

// NodeCertificateChecker asks the environment's proxy nodes for the
// node-local certificate state of one domain.
type NodeCertificateChecker func(ctx context.Context, domain string) NodeCertificateEvidence

// nodeCertificateRequest performs one read-only GET against a node's takod.
type nodeCertificateRequest func(ctx context.Context, server string, endpoint string) (string, error)

const nodeCertificateRequestTimeout = 10 * time.Second

// NewTakodNodeCertificateChecker builds a checker over the given proxy
// servers using the runtime factory. Capability probes are cached per server
// for the checker's lifetime.
func NewTakodNodeCertificateChecker(factory *nodeclient.Factory, socket string, servers []string) NodeCertificateChecker {
	if factory == nil || len(servers) == 0 {
		return nil
	}
	return newNodeCertificateChecker(servers, func(ctx context.Context, server string, endpoint string) (string, error) {
		client, _, err := factory.Client(ctx, server)
		if err != nil {
			return "", err
		}
		return takodclient.RequestJSONWithContext(ctx, client, socket, http.MethodGet, endpoint, nil)
	})
}

// DomainNodeCertificateChecker returns a checker over the environment's proxy
// nodes when at least one spec declares proxy.cdn, and nil otherwise so
// non-CDN domain checks never contact the nodes.
func DomainNodeCertificateChecker(cfg *config.Config, envName string, factory *nodeclient.Factory, specs []DomainStatusSpec) NodeCertificateChecker {
	if cfg == nil || factory == nil {
		return nil
	}
	declared := false
	for _, spec := range specs {
		if normalizeDeclaredCDN(spec.CDN) != "" {
			declared = true
			break
		}
	}
	if !declared {
		return nil
	}
	servers, err := cfg.GetEnvironmentProxyServers(envName)
	if err != nil {
		return nil
	}
	return NewTakodNodeCertificateChecker(factory, TakodSocketFromConfig(cfg), servers)
}

func newNodeCertificateChecker(servers []string, request nodeCertificateRequest) NodeCertificateChecker {
	servers = append([]string(nil), servers...)
	sort.Strings(servers)
	var mu sync.Mutex
	capable := make(map[string]bool, len(servers))
	return func(ctx context.Context, domain string) NodeCertificateEvidence {
		evidence := NodeCertificateEvidence{Servers: servers}
		if len(servers) == 0 {
			evidence.Detail = "no proxy nodes to ask"
			return evidence
		}
		var missing []string
		for _, server := range servers {
			mu.Lock()
			supported, known := capable[server]
			mu.Unlock()
			if !known {
				probeCtx, cancel := context.WithTimeout(ctx, nodeCertificateRequestTimeout)
				output, err := request(probeCtx, server, "/v1/status")
				cancel()
				if err != nil {
					evidence.Detail = fmt.Sprintf("cannot ask %s for its certificate: %v", server, err)
					return evidence
				}
				var status struct {
					Capabilities []string `json:"capabilities"`
				}
				if err := json.Unmarshal([]byte(output), &status); err != nil {
					evidence.Detail = fmt.Sprintf("invalid takod status from %s: %v", server, err)
					return evidence
				}
				supported = false
				for _, capability := range status.Capabilities {
					if capability == takod.CapabilityProxyCertStatusV1 {
						supported = true
						break
					}
				}
				mu.Lock()
				capable[server] = supported
				mu.Unlock()
			}
			if !supported {
				evidence.Detail = fmt.Sprintf("takod on %s does not report node certificate status (%s); run 'tako upgrade servers'", server, takod.CapabilityProxyCertStatusV1)
				return evidence
			}
		}
		evidence.Supported = true
		for _, server := range servers {
			requestCtx, cancel := context.WithTimeout(ctx, nodeCertificateRequestTimeout)
			output, err := request(requestCtx, server, takodclient.CertificateStatusEndpoint(domain))
			cancel()
			if err != nil {
				evidence.Supported = false
				evidence.Detail = fmt.Sprintf("cannot read certificate status from %s: %v", server, err)
				return evidence
			}
			var status takod.ProxyCertificateStatusResponse
			if err := json.Unmarshal([]byte(output), &status); err != nil {
				evidence.Supported = false
				evidence.Detail = fmt.Sprintf("invalid certificate status from %s: %v", server, err)
				return evidence
			}
			if !status.Valid {
				switch {
				case !status.Present:
					missing = append(missing, server+": no certificate yet")
				case status.Error != "":
					missing = append(missing, server+": "+status.Error)
				default:
					missing = append(missing, server+": certificate is not valid")
				}
				continue
			}
			if evidence.Source == "" {
				evidence.Source, evidence.Issuer = status.Source, status.Issuer
			}
			if evidence.NotAfter.IsZero() || status.NotAfter.Before(evidence.NotAfter) {
				evidence.NotAfter = status.NotAfter
			}
		}
		if len(missing) > 0 {
			evidence.Detail = strings.Join(missing, "; ")
			return evidence
		}
		evidence.Valid = true
		return evidence
	}
}

// applyDomainOriginCertificate resolves a CDN-fronted origin whose public TLS
// probe failed. With proxy.cdn declared, port 443 may accept only the CDN's
// address ranges, so the probe from this machine can never succeed; the
// proxy nodes' own certificate state decides instead. Domains without a
// declared CDN, and every state other than pending_tls, are left unchanged.
func applyDomainOriginCertificate(ctx context.Context, status *health.DomainStatus, spec DomainStatusSpec, checker NodeCertificateChecker) {
	if status == nil || status.State != health.DomainStatePendingTLS || normalizeDeclaredCDN(spec.CDN) == "" {
		return
	}
	probe := "the public TLS probe from this machine did not succeed"
	if status.TLSError != "" {
		probe += " (" + status.TLSError + ")"
	}
	restricted := "proxy.cdn is declared, so the origin may accept only the CDN's address ranges on port 443"
	if checker == nil {
		status.Message = fmt.Sprintf("DNS is routed, but %s; %s and node certificate status is unavailable", probe, restricted)
		return
	}
	evidence := checker(ctx, status.Domain)
	if !evidence.Supported {
		status.Message = fmt.Sprintf("DNS is routed, but %s; %s and node certificate status is unavailable: %s", probe, restricted, evidence.Detail)
		return
	}
	if !evidence.Valid {
		status.Message = "DNS is routed; waiting for TLS certificate issuance on the proxy node(s): " + evidence.Detail
		return
	}
	status.State = health.DomainStateActive
	status.TLS = health.DomainTLSActive
	status.OriginRestricted = true
	status.NodeCertificate = &health.NodeCertificateInfo{
		Servers:  append([]string(nil), evidence.Servers...),
		Source:   evidence.Source,
		Issuer:   evidence.Issuer,
		NotAfter: evidence.NotAfter,
	}
	status.Message = fmt.Sprintf("certificate is issued and valid on the proxy node(s); %s, which is expected because %s", probe, restricted)
}

// DomainStateLabel renders a domain state for humans, marking
// origin-restricted domains.
func DomainStateLabel(status health.DomainStatus) string {
	if status.OriginRestricted && status.State == health.DomainStateActive {
		return string(status.State) + " (origin-restricted)"
	}
	return string(status.State)
}
