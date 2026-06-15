package ssh

import (
	"strings"
	"testing"
)

func TestBuildControlPathStaysShortForLongHostAndUser(t *testing.T) {
	mux := NewMultiplexer()
	host := strings.Repeat("very-long-subdomain.", 8) + "example.com"
	user := strings.Repeat("deploy-user-", 8)

	path := mux.buildControlPath(host, 22222, user)
	if len(path) > 100 {
		t.Fatalf("control path length = %d, want <= 100: %s", len(path), path)
	}
	if strings.Contains(path, host) || strings.Contains(path, user) {
		t.Fatalf("control path should not embed raw host/user: %s", path)
	}
	if !strings.Contains(path, "mux-") {
		t.Fatalf("control path should contain mux prefix: %s", path)
	}
}
