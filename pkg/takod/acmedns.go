package takod

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultAcmeDNSImage     = "joohoi/acme-dns:v1.0"
	acmeDNSContainerName    = "tako-acme-dns"
	acmeDNSCredentialsFile  = "credentials.json"
	acmeDNSDefaultDomain    = "acme.tako"
	acmeDNSDefaultNSAdmin   = "admin.tako"
	acmeDNSDefaultAPIListen = "0.0.0.0"
)

var (
	acmeDNSDataDir   = "/data/tako/acme-dns"
	acmeDNSConfigDir = "/data/tako/acme-dns/config"
)

type ReconcileAcmeDNSRequest struct {
	ServerIP string `json:"serverIP"`
	Image    string `json:"image,omitempty"`
}

type ReconcileAcmeDNSResponse struct {
	Container string `json:"container"`
	Image     string `json:"image"`
}

type AcmeDNSCredentialsRequest struct {
	Content string `json:"content"`
}

type AcmeDNSCredentialsResponse struct {
	Content string `json:"content"`
}

func ReconcileAcmeDNS(ctx context.Context, req ReconcileAcmeDNSRequest) (*ReconcileAcmeDNSResponse, error) {
	req.ServerIP = strings.TrimSpace(req.ServerIP)
	if req.ServerIP == "" || (net.ParseIP(req.ServerIP) == nil && !isSafeAcmeDNSHostname(req.ServerIP)) {
		return nil, fmt.Errorf("serverIP must be a valid IP address or hostname")
	}
	if req.Image == "" {
		req.Image = defaultAcmeDNSImage
	}
	if err := ensureAcmeDNSDirectories(); err != nil {
		return nil, err
	}
	if err := writeAcmeDNSConfig(req.ServerIP); err != nil {
		return nil, err
	}

	running, err := runDocker(ctx, "ps", "--filter", "name=^"+acmeDNSContainerName+"$", "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("failed to check acme-dns container status: %w", err)
	}
	if strings.TrimSpace(running) == acmeDNSContainerName {
		return &ReconcileAcmeDNSResponse{Container: acmeDNSContainerName, Image: req.Image}, nil
	}

	_, _ = runDocker(ctx, "rm", "-f", acmeDNSContainerName)
	if output, err := runDocker(ctx, buildAcmeDNSContainerArgs(req.Image)...); err != nil {
		return nil, fmt.Errorf("failed to start acme-dns: %w, output: %s", err, output)
	}
	return &ReconcileAcmeDNSResponse{Container: acmeDNSContainerName, Image: req.Image}, nil
}

func isSafeAcmeDNSHostname(hostname string) bool {
	if len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i, r := range label {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				continue
			}
			if r == '-' && i != 0 && i != len(label)-1 {
				continue
			}
			return false
		}
	}
	return true
}

func RemoveAcmeDNS(ctx context.Context) (*ReconcileAcmeDNSResponse, error) {
	if _, err := runDocker(ctx, "rm", "-f", acmeDNSContainerName); err != nil {
		return nil, fmt.Errorf("failed to remove acme-dns: %w", err)
	}
	return &ReconcileAcmeDNSResponse{Container: acmeDNSContainerName, Image: defaultAcmeDNSImage}, nil
}

func ReadAcmeDNSCredentials() (*AcmeDNSCredentialsResponse, error) {
	path := filepath.Join(acmeDNSDataDir, acmeDNSCredentialsFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &AcmeDNSCredentialsResponse{Content: ""}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read acme-dns credentials: %w", err)
	}
	return &AcmeDNSCredentialsResponse{Content: string(data)}, nil
}

func WriteAcmeDNSCredentials(req AcmeDNSCredentialsRequest) (*AcmeDNSCredentialsResponse, error) {
	if err := os.MkdirAll(acmeDNSDataDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create acme-dns data directory: %w", err)
	}
	path := filepath.Join(acmeDNSDataDir, acmeDNSCredentialsFile)
	if err := os.WriteFile(path, []byte(req.Content), 0600); err != nil {
		return nil, fmt.Errorf("failed to write acme-dns credentials: %w", err)
	}
	return &AcmeDNSCredentialsResponse{Content: req.Content}, nil
}

func ensureAcmeDNSDirectories() error {
	for _, dir := range []string{acmeDNSDataDir, acmeDNSConfigDir, filepath.Join(acmeDNSDataDir, "data")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	return nil
}

func writeAcmeDNSConfig(serverIP string) error {
	config := fmt.Sprintf(`[general]
listen = "0.0.0.0:53"
protocol = "both"
domain = "%s"
nsname = "%s"
nsadmin = "%s"
records = [
    "%s. A %s",
    "%s. NS %s."
]
debug = false

[database]
engine = "sqlite3"
connection = "/var/lib/acme-dns/acme-dns.db"

[api]
ip = "%s"
port = "80"
tls = "none"
corsorigins = ["*"]
use_header = false

[logconfig]
loglevel = "info"
logtype = "stdout"
logformat = "text"
`,
		acmeDNSDefaultDomain,
		acmeDNSDefaultDomain,
		acmeDNSDefaultNSAdmin,
		acmeDNSDefaultDomain,
		serverIP,
		acmeDNSDefaultDomain,
		acmeDNSDefaultDomain,
		acmeDNSDefaultAPIListen,
	)
	path := filepath.Join(acmeDNSConfigDir, "config.cfg")
	if err := os.WriteFile(path, []byte(config), 0644); err != nil {
		return fmt.Errorf("failed to write acme-dns config: %w", err)
	}
	return nil
}

func buildAcmeDNSContainerArgs(image string) []string {
	return []string{
		"run", "-d",
		"--name", acmeDNSContainerName,
		"--restart", "unless-stopped",
		"--publish", "53:53/udp",
		"--publish", "53:53/tcp",
		"--publish", "8053:80",
		"--volume", filepath.Join(acmeDNSDataDir, "config") + ":/etc/acme-dns:ro",
		"--volume", filepath.Join(acmeDNSDataDir, "data") + ":/var/lib/acme-dns",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=acme-dns",
		image,
	}
}
