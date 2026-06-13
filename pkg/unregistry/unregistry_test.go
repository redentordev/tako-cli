package unregistry

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestRemoteSSHCommandUsesTOFUKnownHosts(t *testing.T) {
	cmd := remoteSSHCommand(config.ServerConfig{SSHKey: "/home/deploy/.ssh/id_ed25519"})

	for _, expected := range []string{
		"StrictHostKeyChecking=accept-new",
		"UserKnownHostsFile=~/.tako/known_hosts",
		"-i '/home/deploy/.ssh/id_ed25519'",
	} {
		if !strings.Contains(cmd, expected) {
			t.Fatalf("ssh command missing %q: %s", expected, cmd)
		}
	}
	if strings.Contains(cmd, "StrictHostKeyChecking=no") || strings.Contains(cmd, "UserKnownHostsFile=/dev/null") {
		t.Fatalf("ssh command disables host key checking: %s", cmd)
	}
}
