package ssl

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestDetectRequirementsReadsPortLevelProxy(t *testing.T) {
	reqs := DetectRequirements(map[string]config.ServiceConfig{
		"web": {
			Ports: []config.PortConfig{
				{Name: "http", Target: 3000, Mode: "proxy", Protocol: "http", Proxy: &config.ProxyConfig{Domain: "app.example.com"}},
				{Name: "metrics", Target: 9090, Mode: "internal", Protocol: "tcp"},
			},
		},
	})

	if len(reqs) != 1 {
		t.Fatalf("requirements = %#v, want one port-level proxy requirement", reqs)
	}
	if reqs[0].Domain != "app.example.com" || reqs[0].ServiceName != "web" || reqs[0].ChallengeType != ChallengeHTTP01 {
		t.Fatalf("requirement = %#v, want app.example.com HTTP-01", reqs[0])
	}
}
