package ssl

import (
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
)

// ChallengeType represents the ACME challenge type
type ChallengeType string

const (
	// ChallengeHTTP01 uses HTTP-01 challenge (standard, no wildcards)
	ChallengeHTTP01 ChallengeType = "http-01"
	// ChallengeDNS01 uses DNS-01 challenge (required for wildcards)
	ChallengeDNS01 ChallengeType = "dns-01"
)

// DomainRequirement represents SSL requirements for a domain
type DomainRequirement struct {
	Domain        string
	IsWildcard    bool
	BaseDomain    string // "example.com" for "*.example.com"
	ChallengeType ChallengeType
	ServiceName   string
}

// DetectRequirements analyzes all service domains and returns SSL requirements
func DetectRequirements(services map[string]config.ServiceConfig) []DomainRequirement {
	var reqs []DomainRequirement

	for name, svc := range services {
		if svc.Proxy == nil {
			continue
		}

		domains := svc.Proxy.GetAllDomains()
		for _, domain := range domains {
			if domain == "" {
				continue
			}

			req := DomainRequirement{
				Domain:      domain,
				ServiceName: name,
			}

			if IsWildcard(domain) {
				req.IsWildcard = true
				req.BaseDomain = strings.TrimPrefix(domain, "*.")
				req.ChallengeType = ChallengeDNS01
			} else {
				req.IsWildcard = false
				req.BaseDomain = extractBaseDomain(domain)
				req.ChallengeType = ChallengeHTTP01
			}

			reqs = append(reqs, req)
		}
	}

	return reqs
}

// IsWildcard checks if a domain is a wildcard domain
func IsWildcard(domain string) bool {
	return strings.HasPrefix(domain, "*.")
}

// extractBaseDomain extracts the base domain from a full domain
// e.g., "app.example.com" -> "example.com"
func extractBaseDomain(domain string) string {
	parts := strings.Split(domain, ".")
	if len(parts) <= 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// GroupWildcards returns unique wildcard base domains
func GroupWildcards(reqs []DomainRequirement) []string {
	seen := make(map[string]bool)
	var wildcards []string

	for _, req := range reqs {
		if req.IsWildcard && !seen[req.BaseDomain] {
			seen[req.BaseDomain] = true
			wildcards = append(wildcards, req.BaseDomain)
		}
	}

	return wildcards
}

// HasWildcards checks if any requirements include wildcards
func HasWildcards(reqs []DomainRequirement) bool {
	for _, req := range reqs {
		if req.IsWildcard {
			return true
		}
	}
	return false
}

// FilterByChallenge returns requirements matching the specified challenge type
func FilterByChallenge(reqs []DomainRequirement, challengeType ChallengeType) []DomainRequirement {
	var filtered []DomainRequirement
	for _, req := range reqs {
		if req.ChallengeType == challengeType {
			filtered = append(filtered, req)
		}
	}
	return filtered
}
