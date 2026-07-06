package engine

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/health"
)

func TestDomainStatusStrictErrorOnlyFailsPending(t *testing.T) {
	active := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStateActive}}
	if err := domainStatusStrictError(active, true); err != nil {
		t.Fatalf("active strict status returned error: %v", err)
	}

	pending := []health.DomainStatus{{Domain: "app.example.com", State: health.DomainStatePendingDNS}}
	err := domainStatusStrictError(pending, true)
	if err == nil {
		t.Fatal("pending strict status returned nil")
	}
	if !strings.Contains(err.Error(), "app.example.com=pending_dns") {
		t.Fatalf("error = %q", err)
	}
	if Classify(err) != ClassAttention {
		t.Fatalf("Classify(%v) = %d, want ClassAttention", err, Classify(err))
	}
}
