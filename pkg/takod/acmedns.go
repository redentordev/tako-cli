package takod

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultAcmeDNSImage     = "joohoi/acme-dns:v1.0"
	acmeDNSContainerName    = "tako-acme-dns"
	acmeDNSCredentialsFile  = "credentials.json"
	acmeDNSDefaultDomain    = "acme.tako"
	acmeDNSDefaultNSAdmin   = "admin.tako"
	acmeDNSDefaultAPIListen = "0.0.0.0"
	acmeDNSHostAPIBinding   = "127.0.0.1:8053:80"
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

type AcmeDNSRegisterRequest struct {
	Domain   string `json:"domain"`
	ServerIP string `json:"serverIP"`
}

type AcmeDNSRegistration struct {
	Domain      string    `json:"domain"`
	Subdomain   string    `json:"subdomain"`
	Username    string    `json:"username"`
	Password    string    `json:"password"`
	FullDomain  string    `json:"fulldomain"`
	CNAMETarget string    `json:"cname_target"`
	ServerIP    string    `json:"server_ip"`
	CreatedAt   time.Time `json:"created_at"`
}

type AcmeDNSRegisterResponse struct {
	Registration AcmeDNSRegistration `json:"registration"`
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
	configChanged, err := writeAcmeDNSConfig(req.ServerIP)
	if err != nil {
		return nil, err
	}

	running, err := runDocker(ctx, "ps", "--filter", "name=^"+acmeDNSContainerName+"$", "--format", "{{.Names}}")
	if err != nil {
		return nil, fmt.Errorf("failed to check acme-dns container status: %w", err)
	}
	if strings.TrimSpace(running) == acmeDNSContainerName {
		if !configChanged && acmeDNSContainerHasLoopbackAPI(ctx) {
			return &ReconcileAcmeDNSResponse{Container: acmeDNSContainerName, Image: req.Image}, nil
		}
		if _, err := runDocker(ctx, "rm", "-f", acmeDNSContainerName); err != nil {
			return nil, fmt.Errorf("failed to replace acme-dns container: %w", err)
		}
	}

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

func RegisterAcmeDNS(ctx context.Context, req AcmeDNSRegisterRequest) (*AcmeDNSRegisterResponse, error) {
	req.Domain = strings.TrimSpace(req.Domain)
	if req.Domain == "" || !isSafeAcmeDNSHostname(req.Domain) {
		return nil, fmt.Errorf("domain must be a valid hostname")
	}
	req.ServerIP = strings.TrimSpace(req.ServerIP)
	if req.ServerIP == "" || (net.ParseIP(req.ServerIP) == nil && !isSafeAcmeDNSHostname(req.ServerIP)) {
		return nil, fmt.Errorf("serverIP must be a valid IP address or hostname")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:8053/register", nil)
	if err != nil {
		return nil, err
	}
	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to register with acme-dns: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("failed to read acme-dns response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("acme-dns register returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var apiResponse struct {
		Subdomain  string `json:"subdomain"`
		Username   string `json:"username"`
		Password   string `json:"password"`
		FullDomain string `json:"fulldomain"`
	}
	if err := json.Unmarshal(body, &apiResponse); err != nil {
		return nil, fmt.Errorf("failed to parse acme-dns response: %w", err)
	}
	registration := AcmeDNSRegistration{
		Domain:      req.Domain,
		Subdomain:   apiResponse.Subdomain,
		Username:    apiResponse.Username,
		Password:    apiResponse.Password,
		FullDomain:  apiResponse.FullDomain,
		CNAMETarget: apiResponse.FullDomain,
		ServerIP:    req.ServerIP,
		CreatedAt:   time.Now().UTC(),
	}
	if err := saveAcmeDNSRegistration(registration); err != nil {
		return nil, err
	}
	return &AcmeDNSRegisterResponse{Registration: registration}, nil
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
	if err := writeFileAtomic(path, []byte(req.Content), 0600); err != nil {
		return nil, fmt.Errorf("failed to write acme-dns credentials: %w", err)
	}
	return &AcmeDNSCredentialsResponse{Content: req.Content}, nil
}

func saveAcmeDNSRegistration(registration AcmeDNSRegistration) error {
	creds := struct {
		Registrations map[string]AcmeDNSRegistration `json:"registrations"`
	}{
		Registrations: map[string]AcmeDNSRegistration{},
	}
	current, err := ReadAcmeDNSCredentials()
	if err != nil {
		return err
	}
	if strings.TrimSpace(current.Content) != "" {
		_ = json.Unmarshal([]byte(current.Content), &creds)
	}
	if creds.Registrations == nil {
		creds.Registrations = map[string]AcmeDNSRegistration{}
	}
	creds.Registrations[registration.Domain] = registration

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	_, err = WriteAcmeDNSCredentials(AcmeDNSCredentialsRequest{Content: string(data)})
	return err
}

func ensureAcmeDNSDirectories() error {
	for _, dir := range []string{acmeDNSDataDir, acmeDNSConfigDir, filepath.Join(acmeDNSDataDir, "data")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	return nil
}

func writeAcmeDNSConfig(serverIP string) (bool, error) {
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
	if current, err := os.ReadFile(path); err == nil && string(current) == config {
		return false, nil
	}
	if err := writeFileAtomic(path, []byte(config), 0644); err != nil {
		return false, fmt.Errorf("failed to write acme-dns config: %w", err)
	}
	return true, nil
}

func buildAcmeDNSContainerArgs(image string) []string {
	return []string{
		"run", "-d",
		"--name", acmeDNSContainerName,
		"--restart", "unless-stopped",
		"--publish", "53:53/udp",
		"--publish", "53:53/tcp",
		"--publish", acmeDNSHostAPIBinding,
		"--volume", filepath.Join(acmeDNSDataDir, "config") + ":/etc/acme-dns:ro",
		"--volume", filepath.Join(acmeDNSDataDir, "data") + ":/var/lib/acme-dns",
		"--label", "tako.runtime=takod",
		"--label", "tako.component=acme-dns",
		image,
	}
}

type acmeDNSDockerPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

func acmeDNSContainerHasLoopbackAPI(ctx context.Context) bool {
	output, err := runDocker(ctx, "inspect", acmeDNSContainerName, "--format", "{{json .HostConfig.PortBindings}}")
	if err != nil {
		return false
	}
	return acmeDNSPortBindingsHaveLoopbackAPI(output)
}

func acmeDNSPortBindingsHaveLoopbackAPI(output string) bool {
	var bindings map[string][]acmeDNSDockerPortBinding
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &bindings); err != nil {
		return false
	}
	for _, binding := range bindings["80/tcp"] {
		if binding.HostPort == "8053" && binding.HostIP == "127.0.0.1" {
			return true
		}
	}
	return false
}
