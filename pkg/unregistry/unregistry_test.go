package unregistry

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

func TestRemoteSSHCommandUsesTOFUKnownHosts(t *testing.T) {
	ssh.SetGlobalHostKeyMode(ssh.HostKeyModeTOFU)
	t.Cleanup(func() {
		ssh.SetGlobalHostKeyMode(ssh.HostKeyModeTOFU)
	})

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

func TestRemoteSSHCommandHonorsStrictHostKeyMode(t *testing.T) {
	ssh.SetGlobalHostKeyMode(ssh.HostKeyModeStrict)
	t.Cleanup(func() {
		ssh.SetGlobalHostKeyMode(ssh.HostKeyModeTOFU)
	})

	cmd := remoteSSHCommand(config.ServerConfig{SSHKey: "/home/deploy/.ssh/id_ed25519"})
	if !strings.Contains(cmd, "StrictHostKeyChecking=yes") {
		t.Fatalf("strict host key mode was not applied: %s", cmd)
	}
	if strings.Contains(cmd, "StrictHostKeyChecking=accept-new") {
		t.Fatalf("strict host key mode should not accept new host keys: %s", cmd)
	}
}

func TestBuildTakodImageStreamCommandUsesTakodEndpoints(t *testing.T) {
	cmd := buildTakodImageStreamCommand("/run/tako/takod.sock", config.ServerConfig{
		Host:   "10.210.0.2",
		Port:   2222,
		User:   "deploy",
		SSHKey: "/home/deploy/.ssh/id_ed25519",
	}, "demo/web:abc123")

	for _, expected := range []string{
		"--unix-socket '/run/tako/takod.sock'",
		"/v1/images/export?image=demo%2Fweb%3Aabc123",
		"/v1/images/import?image=demo%2Fweb%3Aabc123",
		"--data-binary @-",
		"deploy@10.210.0.2",
		"-p 2222",
	} {
		if !strings.Contains(cmd, expected) {
			t.Fatalf("stream command missing %q: %s", expected, cmd)
		}
	}
	for _, unexpected := range []string{"docker save", "docker load", "docker image"} {
		if strings.Contains(cmd, unexpected) {
			t.Fatalf("stream command should not run %q directly: %s", unexpected, cmd)
		}
	}
}
