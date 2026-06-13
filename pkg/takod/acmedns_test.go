package takod

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBuildAcmeDNSContainerArgs(t *testing.T) {
	got := buildAcmeDNSContainerArgs("joohoi/acme-dns:v1.0")
	want := []string{
		"run", "-d",
		"--name", "tako-acme-dns",
		"--restart", "unless-stopped",
		"--publish", "53:53/udp",
		"--publish", "53:53/tcp",
		"--publish", "127.0.0.1:8053:80",
		"--volume", "/data/tako/acme-dns/config:/etc/acme-dns:ro",
		"--volume", "/data/tako/acme-dns/data:/var/lib/acme-dns",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=acme-dns",
		"joohoi/acme-dns:v1.0",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected acme-dns args:\ngot:  %#v\nwant: %#v", got, want)
	}
}

func TestAcmeDNSPortBindingsRequireLoopbackAPI(t *testing.T) {
	loopback := `{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8053"}]}`
	if !acmeDNSPortBindingsHaveLoopbackAPI(loopback) {
		t.Fatal("expected loopback API binding to be accepted")
	}

	public := `{"80/tcp":[{"HostIp":"0.0.0.0","HostPort":"8053"}]}`
	if acmeDNSPortBindingsHaveLoopbackAPI(public) {
		t.Fatal("expected public API binding to be rejected")
	}

	emptyHost := `{"80/tcp":[{"HostIp":"","HostPort":"8053"}]}`
	if acmeDNSPortBindingsHaveLoopbackAPI(emptyHost) {
		t.Fatal("expected wildcard Docker API binding to be rejected")
	}
}

func TestAcmeDNSCredentialsRoundTrip(t *testing.T) {
	oldDataDir := acmeDNSDataDir
	oldConfigDir := acmeDNSConfigDir
	acmeDNSDataDir = t.TempDir()
	acmeDNSConfigDir = filepath.Join(acmeDNSDataDir, "config")
	t.Cleanup(func() {
		acmeDNSDataDir = oldDataDir
		acmeDNSConfigDir = oldConfigDir
	})

	if _, err := WriteAcmeDNSCredentials(AcmeDNSCredentialsRequest{Content: `{"registrations":{}}`}); err != nil {
		t.Fatalf("WriteAcmeDNSCredentials returned error: %v", err)
	}
	response, err := ReadAcmeDNSCredentials()
	if err != nil {
		t.Fatalf("ReadAcmeDNSCredentials returned error: %v", err)
	}
	if response.Content != `{"registrations":{}}` {
		t.Fatalf("content = %q", response.Content)
	}

	info, err := os.Stat(filepath.Join(acmeDNSDataDir, acmeDNSCredentialsFile))
	if err != nil {
		t.Fatalf("failed to stat credentials file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("credentials mode = %04o, want 0600", info.Mode().Perm())
	}
}

func TestIsSafeAcmeDNSHostname(t *testing.T) {
	for _, hostname := range []string{"example.com", "acme-1.example.com"} {
		if !isSafeAcmeDNSHostname(hostname) {
			t.Fatalf("expected %q to be accepted", hostname)
		}
	}
	for _, hostname := range []string{"", "-example.com", "example-.com", "bad_host", "bad;host"} {
		if isSafeAcmeDNSHostname(hostname) {
			t.Fatalf("expected %q to be rejected", hostname)
		}
	}
}
