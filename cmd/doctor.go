package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/nixpacks"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/redentordev/tako-cli/pkg/takodstate"
	"github.com/redentordev/tako-cli/pkg/utils"
	"github.com/spf13/cobra"
)

var doctorSkipRemote bool

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check project health and diagnose common issues",
	Long: `Run health checks on your Tako project to identify missing files,
broken SSH connections, missing secrets, and other common issues.

Checks performed:
  - Configuration file (tako.yaml / tako.json)
  - Environment file (.env)
  - SSH key accessibility and permissions
  - Secrets configuration
  - Local state (.tako directory)
  - Local build inputs
  - Server connectivity (skip with --skip-remote)
  - Server agent version (skip with --skip-remote)
  - Node agent health: takod /healthz, mesh status, disk pressure (skip with --skip-remote)
  - Docker runtime and proxy runtime (skip with --skip-remote)
  - Replicated deployment/runtime state (skip with --skip-remote)
  - Running services (skip with --skip-remote)

Examples:
  tako doctor                  # Run all checks
  tako doctor --skip-remote    # Skip SSH and service checks`,
	RunE: runDoctor,
}

func init() {
	rootCmd.AddCommand(doctorCmd)
	doctorCmd.Flags().BoolVar(&doctorSkipRemote, "skip-remote", false, "Skip SSH connectivity and service checks")
}

type checkResult struct {
	status  string // "PASS", "WARN", "FAIL"
	message string
	fix     string
}

func runDoctor(cmd *cobra.Command, args []string) error {
	passed, warned, failed := 0, 0, 0
	result := engine.DoctorResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindDoctorResult,
		SkipRemote: doctorSkipRemote,
		Checks:     []engine.DoctorCheck{},
	}

	// Machine modes reserve stdout for parseable output.
	var out io.Writer = os.Stdout
	if machineOutputEnabled() {
		out = os.Stderr
	}

	section := ""
	heading := func(name string) {
		section = name
		if len(result.Checks) == 0 {
			fmt.Fprintf(out, "=== %s ===\n", name)
		} else {
			fmt.Fprintf(out, "\n=== %s ===\n", name)
		}
	}
	record := func(r checkResult) {
		switch r.status {
		case "PASS":
			passed++
		case "WARN":
			warned++
		case "FAIL":
			failed++
		}
		result.Checks = append(result.Checks, engine.DoctorCheck{
			Name:        section,
			Status:      strings.ToLower(r.status),
			Detail:      r.message,
			Remediation: r.fix,
		})
		fmt.Fprintf(out, "  [%s] %s", r.status, r.message)
		if r.fix != "" {
			fmt.Fprintf(out, " (Fix: %s)", r.fix)
		}
		fmt.Fprintln(out)
	}
	finish := func() error {
		fmt.Fprintf(out, "\nSummary: %d passed, %d warning(s), %d failed\n", passed, warned, failed)
		result.Passed, result.Warned, result.Failed = passed, warned, failed
		result.Status = "ok"
		if failed > 0 {
			result.Status = "attention"
		}
		if err := emitResultDocument(result); err != nil {
			return err
		}
		if failed > 0 {
			return &engine.AttentionError{Err: fmt.Errorf("doctor found %d issue(s)", failed)}
		}
		return nil
	}

	heading("Configuration")
	cfg, cfgErr := checkConfig(record)

	if cfgErr != nil {
		return finish()
	}
	result.Project = cfg.Project.Name

	envName := getEnvironmentName(cfg)
	result.Environment = envName

	heading("Environment")
	checkEnvFile(record)

	heading("SSH Keys")
	checkSSHKeys(record, cfg, envName)

	heading("Secrets")
	checkSecrets(record, cfg, envName)

	heading("Local State")
	checkLocalState(record)

	heading("Local Build Inputs")
	checkLocalBuildInputs(record, cfg, envName)

	remoteSections := []string{
		"Server Connectivity",
		"Server Agent Version",
		"Node Agent Health",
		"Docker Runtime",
		"Proxy Runtime",
		"Running Services",
		"Replicated State",
		"External Volumes",
	}
	if !doctorSkipRemote {
		heading(remoteSections[0])
		clients := checkServerConnectivity(record, cfg, envName)

		heading(remoteSections[1])
		checkServerAgentVersion(record, cfg, clients)

		heading(remoteSections[2])
		checkNodeAgentHealth(record, cfg, clients)

		heading(remoteSections[3])
		checkDockerRuntime(record, clients)

		heading(remoteSections[4])
		checkProxyRuntime(record, cfg, envName, clients)

		heading(remoteSections[5])
		checkRunningServices(record, cfg, envName, clients)

		heading(remoteSections[6])
		checkReplicatedState(record, cfg, envName, clients)

		heading(remoteSections[7])
		checkExternalVolumes(record, cfg, envName, clients)

		// Clean up clients
		for _, c := range clients {
			c.Close()
		}
	} else {
		for _, name := range remoteSections {
			heading(name)
			record(checkResult{"SKIP", "Skipped (--skip-remote)", ""})
		}
	}

	return finish()
}

// Node agent health probes use a short timeout so a wedged takod cannot
// stall the whole diagnosis.
const doctorNodeProbeTimeout = 3 * time.Second

// Disk pressure thresholds for the root filesystem, in percent used.
const (
	doctorDiskWarnPercent = 85
	doctorDiskFailPercent = 95
)

// doctorNodeHealthInfo carries one node's agent-health probe results:
// takod socket health (/healthz), mesh peer status (/v1/mesh/status), and
// root-filesystem disk pressure.
type doctorNodeHealthInfo struct {
	HealthzErr      string
	Mesh            *mesh.Status
	MeshErr         string
	DiskUsedPercent int // -1 when unknown
	DiskErr         string
}

func checkNodeAgentHealth(record func(checkResult), cfg *config.Config, clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check node agent health", ""})
		return
	}

	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)

	checkNodeAgentHealthWith(record, clientNames, cfg.Mesh != nil, func(clientName string) (*doctorNodeHealthInfo, error) {
		return detectDoctorNodeHealth(clients[clientName], cfg)
	})
}

func checkNodeAgentHealthWith(record func(checkResult), clientNames []string, meshConfigured bool, probe func(string) (*doctorNodeHealthInfo, error)) {
	for _, clientName := range clientNames {
		info, err := probe(clientName)
		if err != nil || info == nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot probe node agent health: %v", clientName, err), "Run 'tako setup' or check 'systemctl status takod' on the node"})
			continue
		}

		if info.HealthzErr != "" {
			record(checkResult{"FAIL", fmt.Sprintf("%s: takod /healthz unreachable: %s", clientName, info.HealthzErr), "Run 'tako setup' or check 'systemctl status takod' on the node"})
		} else {
			record(checkResult{"PASS", fmt.Sprintf("%s: takod /healthz ok", clientName), ""})
		}

		if meshConfigured {
			switch {
			case info.MeshErr != "":
				record(checkResult{"WARN", fmt.Sprintf("%s: mesh status unavailable: %s", clientName, info.MeshErr), "Run 'tako deploy' or 'tako state repair' to reapply mesh configuration"})
			case info.Mesh == nil || !info.Mesh.Up:
				record(checkResult{"FAIL", fmt.Sprintf("%s: mesh interface is down", clientName), "Run 'tako deploy' to reapply mesh configuration"})
			default:
				record(checkResult{"PASS", fmt.Sprintf("%s: mesh %s up (%d peer(s))", clientName, info.Mesh.Interface, info.Mesh.Peers), ""})
			}
		}

		switch {
		case info.DiskErr != "" || info.DiskUsedPercent < 0:
			record(checkResult{"WARN", fmt.Sprintf("%s: Cannot read root filesystem usage: %s", clientName, info.DiskErr), ""})
		case info.DiskUsedPercent >= doctorDiskFailPercent:
			record(checkResult{"FAIL", fmt.Sprintf("%s: root filesystem %d%% used", clientName, info.DiskUsedPercent), "Free disk space or run 'tako cleanup' before the next deploy"})
		case info.DiskUsedPercent >= doctorDiskWarnPercent:
			record(checkResult{"WARN", fmt.Sprintf("%s: root filesystem %d%% used", clientName, info.DiskUsedPercent), "Free disk space or run 'tako cleanup'"})
		default:
			record(checkResult{"PASS", fmt.Sprintf("%s: root filesystem %d%% used", clientName, info.DiskUsedPercent), ""})
		}
	}
}

func detectDoctorNodeHealth(client *ssh.Client, cfg *config.Config) (*doctorNodeHealthInfo, error) {
	info := &doctorNodeHealthInfo{DiskUsedPercent: -1}
	socket := takodSocketFromConfig(cfg)

	if output, err := takodclient.RequestJSONWithTimeout(client, socket, "GET", "/healthz", nil, doctorNodeProbeTimeout); err != nil {
		info.HealthzErr = err.Error()
	} else {
		var health struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal([]byte(output), &health); err != nil || !health.OK {
			info.HealthzErr = "unexpected /healthz response"
		}
	}

	if cfg.Mesh != nil {
		path := "/v1/mesh/status?interface=" + url.QueryEscape(cfg.Mesh.Interface)
		if output, err := takodclient.RequestJSONWithTimeout(client, socket, "GET", path, nil, doctorNodeProbeTimeout); err != nil {
			info.MeshErr = err.Error()
		} else {
			var status mesh.Status
			if err := json.Unmarshal([]byte(output), &status); err != nil {
				info.MeshErr = fmt.Sprintf("unreadable mesh status: %v", err)
			} else {
				info.Mesh = &status
			}
		}
	}

	if output, err := client.Execute("df -P / | tail -1"); err != nil {
		info.DiskErr = err.Error()
	} else if percent, err := parseDoctorDiskUsedPercent(output); err != nil {
		info.DiskErr = err.Error()
	} else {
		info.DiskUsedPercent = percent
	}

	return info, nil
}

// parseDoctorDiskUsedPercent extracts the use% column from one `df -P` row.
func parseDoctorDiskUsedPercent(output string) (int, error) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) < 5 {
		return -1, fmt.Errorf("unexpected df output")
	}
	percent, err := strconv.Atoi(strings.TrimSuffix(fields[4], "%"))
	if err != nil {
		return -1, fmt.Errorf("unexpected df use%% value %q", fields[4])
	}
	return percent, nil
}

func checkDockerRuntime(record func(checkResult), clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check Docker runtime", ""})
		return
	}

	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)

	checkDockerRuntimeWith(record, clientNames, func(clientName string) (*provisioner.DockerRuntimeInfo, error) {
		return provisioner.DetectDockerRuntime(clients[clientName])
	})
}

func checkDockerRuntimeWith(record func(checkResult), clientNames []string, probe func(string) (*provisioner.DockerRuntimeInfo, error)) {
	for _, clientName := range clientNames {
		info, err := probe(clientName)
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Docker runtime unsupported - %v", clientName, err), "Install/start rootful system Docker, then rerun 'tako setup'"})
			continue
		}
		if info.Rootless {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Docker runtime is rootless", clientName), "Use rootful system Docker for remote takod nodes"})
			continue
		}
		record(checkResult{"PASS", fmt.Sprintf("%s: Docker rootful daemon %s (root dir: %s)", clientName, dockerRuntimeValue(info.ServerVersion), dockerRuntimeValue(info.RootDir)), ""})
	}
}

func dockerRuntimeValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	return value
}

func checkServerAgentVersion(record func(checkResult), cfg *config.Config, clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check takod agent version", ""})
		return
	}

	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)

	checkServerAgentVersionWith(record, clientNames, Version, func(clientName string) (*takodRemoteStatus, error) {
		return readTakodAgentStatus(clients[clientName], cfg)
	})
}

func checkServerAgentVersionWith(record func(checkResult), clientNames []string, targetVersion string, probe func(string) (*takodRemoteStatus, error)) {
	targetVersion = strings.TrimSpace(targetVersion)
	if targetVersion == "" {
		targetVersion = "unknown"
	}

	for _, clientName := range clientNames {
		status, err := probe(clientName)
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot read takod agent version: %v", clientName, err), "Run 'tako setup' or 'tako upgrade servers' before deploy"})
			continue
		}
		if status == nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot read takod agent version: empty status", clientName), "Run 'tako setup' or 'tako upgrade servers' before deploy"})
			continue
		}

		currentVersion := strings.TrimSpace(status.Version)
		if currentVersion == "" {
			currentVersion = "unknown"
		}
		runtime := strings.TrimSpace(status.Runtime)
		if runtime == "" {
			runtime = "takod"
		}
		hostname := strings.TrimSpace(status.Hostname)
		if hostname == "" {
			hostname = clientName
		}

		if currentVersion == targetVersion {
			record(checkResult{"PASS", fmt.Sprintf("%s: %s %s matches CLI %s on %s", clientName, runtime, currentVersion, targetVersion, hostname), ""})
			continue
		}

		if cliVersionRequiresTakodBinary(targetVersion) {
			record(checkResult{"WARN", fmt.Sprintf("%s: %s %s differs from development CLI %s on %s", clientName, runtime, currentVersion, targetVersion, hostname), "Run 'tako upgrade servers --takod-binary <linux-tako-binary>' before testing this CLI against servers"})
			continue
		}

		record(checkResult{"FAIL", fmt.Sprintf("%s: %s %s differs from CLI %s on %s", clientName, runtime, currentVersion, targetVersion, hostname), "Run 'tako upgrade servers --dry-run', then 'tako upgrade servers'"})
	}
}

type doctorBuildInputInfo struct {
	BuildPath         string
	Dockerfile        string
	Framework         string
	NeedsNixpacks     bool
	NixpacksAvailable bool
}

func checkLocalBuildInputs(record func(checkResult), cfg *config.Config, envName string) {
	checkLocalBuildInputsWith(record, cfg, envName, doctorNixpacksAvailable)
}

func checkLocalBuildInputsWith(record func(checkResult), cfg *config.Config, envName string, nixpacksAvailable func(string) bool) {
	services, err := cfg.GetServices(envName)
	if err != nil {
		record(checkResult{"WARN", fmt.Sprintf("Cannot resolve services for local build input checks: %v", err), ""})
		return
	}

	buildInputs := make(map[string]config.ServiceConfig)
	for name, service := range services {
		if strings.TrimSpace(service.Build) != "" {
			buildInputs[name] = service
		}
	}
	for name, build := range cfg.Builds {
		buildInputs["build:"+name] = config.ServiceConfig{Build: build.Context, BuildArgs: build.Args, BuildTarget: build.Target, Dockerfile: build.Dockerfile}
	}
	buildServiceNames := make([]string, 0, len(buildInputs))
	for name := range buildInputs {
		buildServiceNames = append(buildServiceNames, name)
	}
	sort.Strings(buildServiceNames)

	if len(buildServiceNames) == 0 {
		record(checkResult{"PASS", "No build-backed services; local build inputs not required", ""})
		return
	}

	record(checkResult{"PASS", "Build images stream to remote takod; local Docker daemon is not required for takod deploys", ""})
	for _, serviceName := range buildServiceNames {
		service := buildInputs[serviceName]
		info, err := inspectDoctorBuildInput(serviceName, service, nixpacksAvailable)
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: %v", serviceName, err), "Fix the build context, Dockerfile, or Nixpacks setup before deploy"})
			continue
		}
		if info.NeedsNixpacks && !info.NixpacksAvailable {
			record(checkResult{"FAIL", fmt.Sprintf("%s: build context %s has no Dockerfile; detected %s but Nixpacks is not installed", serviceName, doctorPathDisplay(info.BuildPath), info.Framework), "Install Nixpacks or add a Dockerfile"})
			continue
		}
		if info.NeedsNixpacks {
			record(checkResult{"PASS", fmt.Sprintf("%s: build context %s has %s framework inputs; Nixpacks available to generate Dockerfile", serviceName, doctorPathDisplay(info.BuildPath), info.Framework), ""})
			continue
		}
		record(checkResult{"PASS", fmt.Sprintf("%s: build context %s uses Dockerfile %s", serviceName, doctorPathDisplay(info.BuildPath), info.Dockerfile), ""})
	}
}

func inspectDoctorBuildInput(serviceName string, service config.ServiceConfig, nixpacksAvailable func(string) bool) (*doctorBuildInputInfo, error) {
	buildPath := strings.TrimSpace(service.Build)
	if buildPath == "" {
		return nil, fmt.Errorf("service is not build-backed")
	}

	stat, err := os.Stat(buildPath)
	if err != nil {
		return nil, fmt.Errorf("build path does not exist: %s", service.Build)
	}
	if !stat.IsDir() {
		return nil, fmt.Errorf("build path is not a directory: %s", service.Build)
	}

	if service.Dockerfile != "" {
		dockerfile := filepath.Clean(service.Dockerfile)
		dockerfilePath := filepath.Join(buildPath, dockerfile)
		if stat, err := os.Stat(dockerfilePath); err != nil {
			return nil, fmt.Errorf("dockerfile does not exist: %s", service.Dockerfile)
		} else if stat.IsDir() {
			return nil, fmt.Errorf("dockerfile path is a directory: %s", service.Dockerfile)
		}
		return &doctorBuildInputInfo{BuildPath: buildPath, Dockerfile: dockerfile}, nil
	}

	if dockerfile, ok := findDoctorDockerfile(buildPath); ok {
		return &doctorBuildInputInfo{BuildPath: buildPath, Dockerfile: dockerfile}, nil
	}

	detector := nixpacks.NewDetector(buildPath, false)
	framework, err := detector.DetectFramework()
	if err != nil {
		return nil, fmt.Errorf("build context %s has no Dockerfile and no recognizable Nixpacks framework files", service.Build)
	}
	return &doctorBuildInputInfo{
		BuildPath:         buildPath,
		Framework:         string(framework),
		NeedsNixpacks:     true,
		NixpacksAvailable: nixpacksAvailable(buildPath),
	}, nil
}

func findDoctorDockerfile(buildPath string) (string, bool) {
	for _, candidate := range []string{"Dockerfile", "Dockerfile.prod", "dockerfile", ".dockerfile"} {
		path := filepath.Join(buildPath, candidate)
		if stat, err := os.Stat(path); err == nil && !stat.IsDir() {
			return candidate, true
		}
	}
	return "", false
}

func doctorNixpacksAvailable(buildPath string) bool {
	info, err := nixpacks.NewDetector(buildPath, false).GetFrameworkInfo()
	if err != nil {
		return false
	}
	available, _ := info["nixpacksAvailable"].(bool)
	return available
}

func doctorPathDisplay(path string) string {
	if path == "" {
		return "."
	}
	cleaned := filepath.Clean(path)
	if cwd, err := os.Getwd(); err == nil {
		if absPath, absErr := filepath.Abs(cleaned); absErr == nil {
			if rel, relErr := filepath.Rel(cwd, absPath); relErr == nil && rel != "" && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." {
				return filepath.Clean(rel)
			}
		}
	}
	return cleaned
}

var errDoctorProxyMissing = errors.New("tako-proxy container is not running")

type doctorCommandExecutor interface {
	Execute(command string) (string, error)
}

type doctorProxyRuntimeInfo struct {
	Image     string
	Running   bool
	Args      []string
	Ports     map[string]json.RawMessage
	HostPorts map[string]json.RawMessage
	Mounts    []doctorProxyMount
	Env       []string
}

type doctorProxyMount struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
}

func checkProxyRuntime(record func(checkResult), cfg *config.Config, envName string, clients map[string]*ssh.Client) {
	proxiedServices, err := publicServiceNames(cfg, envName)
	if err != nil {
		record(checkResult{"WARN", fmt.Sprintf("Cannot resolve proxied services: %v", err), ""})
		return
	}
	if len(proxiedServices) == 0 {
		record(checkResult{"PASS", "No proxy routes configured", ""})
		return
	}

	proxyServers, err := cfg.GetEnvironmentProxyServers(envName)
	if err != nil {
		record(checkResult{"WARN", fmt.Sprintf("Cannot resolve proxy servers: %v", err), ""})
		return
	}
	sort.Strings(proxyServers)

	for _, serverName := range proxyServers {
		client, ok := clients[serverName]
		if !ok {
			record(checkResult{"SKIP", fmt.Sprintf("%s: Proxy target not connected", serverName), ""})
			continue
		}
		checkProxyRuntimeWith(record, serverName, proxiedServices, func() (*doctorProxyRuntimeInfo, error) {
			return detectDoctorProxyRuntime(client)
		})
	}
}

func publicServiceNames(cfg *config.Config, envName string) ([]string, error) {
	services, err := cfg.GetServices(envName)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0)
	for name, service := range services {
		if service.IsProxied() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func checkProxyRuntimeWith(record func(checkResult), serverName string, publicServices []string, probe func() (*doctorProxyRuntimeInfo, error)) {
	info, err := probe()
	if errors.Is(err, errDoctorProxyMissing) {
		record(checkResult{"WARN", fmt.Sprintf("%s: tako-proxy is not running for proxied service(s): %s", serverName, strings.Join(publicServices, ", ")), "Run 'tako deploy' to reconcile proxy routes"})
		return
	}
	if err != nil {
		record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot inspect tako-proxy: %v", serverName, err), "Run 'tako deploy' or check Docker on the proxy node"})
		return
	}
	if info == nil {
		record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot inspect tako-proxy: empty result", serverName), "Run 'tako deploy' or check Docker on the proxy node"})
		return
	}
	if !info.Running {
		record(checkResult{"FAIL", fmt.Sprintf("%s: tako-proxy container exists but is not running", serverName), "Run 'tako deploy' to recreate the shared proxy"})
		return
	}

	missing := doctorProxyRuntimeMissing(info)
	if len(missing) > 0 {
		record(checkResult{"FAIL", fmt.Sprintf("%s: tako-proxy is stale or incomplete (missing %s)", serverName, strings.Join(missing, ", ")), "Run 'tako deploy' to recreate the shared proxy"})
		return
	}

	record(checkResult{"PASS", fmt.Sprintf("%s: tako-proxy ready for HTTP/1.1, HTTP/2, HTTP/3 UDP 443, and WebSocket upgrades", serverName), ""})
}

func detectDoctorProxyRuntime(client doctorCommandExecutor) (*doctorProxyRuntimeInfo, error) {
	const command = "sudo docker inspect tako-proxy --format '{{json .State.Running}}{{\"\\n\"}}{{json .Args}}{{\"\\n\"}}{{json .NetworkSettings.Ports}}{{\"\\n\"}}{{json .HostConfig.PortBindings}}{{\"\\n\"}}{{json .Mounts}}{{\"\\n\"}}{{json .Config.Env}}{{\"\\n\"}}{{.Config.Image}}'"
	output, err := client.Execute(command)
	if err != nil {
		combined := strings.ToLower(output + " " + err.Error())
		if strings.Contains(combined, "no such object") {
			return nil, errDoctorProxyMissing
		}
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(output))
	}
	return parseDoctorProxyRuntime(output)
}

func parseDoctorProxyRuntime(output string) (*doctorProxyRuntimeInfo, error) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 6 {
		return nil, fmt.Errorf("unexpected docker inspect output")
	}

	var info doctorProxyRuntimeInfo
	if err := json.Unmarshal([]byte(lines[0]), &info.Running); err != nil {
		return nil, fmt.Errorf("invalid proxy running state: %w", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &info.Args); err != nil {
		return nil, fmt.Errorf("invalid proxy args: %w", err)
	}
	if err := json.Unmarshal([]byte(lines[2]), &info.Ports); err != nil {
		return nil, fmt.Errorf("invalid proxy ports: %w", err)
	}
	offset := 3
	if len(lines) >= 7 {
		if err := json.Unmarshal([]byte(lines[3]), &info.HostPorts); err != nil {
			return nil, fmt.Errorf("invalid proxy host port bindings: %w", err)
		}
		offset = 4
	}
	if err := json.Unmarshal([]byte(lines[offset]), &info.Mounts); err != nil {
		return nil, fmt.Errorf("invalid proxy mounts: %w", err)
	}
	if err := json.Unmarshal([]byte(lines[offset+1]), &info.Env); err != nil {
		return nil, fmt.Errorf("invalid proxy env: %w", err)
	}
	info.Image = strings.TrimSpace(strings.Join(lines[offset+2:], "\n"))
	return &info, nil
}

func doctorProxyRuntimeMissing(info *doctorProxyRuntimeInfo) []string {
	if info == nil {
		return []string{"inspect data"}
	}
	var missing []string
	for _, arg := range []string{
		"run",
		"--config",
		"/etc/caddy/Caddyfile",
		"--adapter",
		"caddyfile",
		"--watch",
	} {
		if !doctorProxyArgExists(info.Args, arg) {
			missing = append(missing, arg)
		}
	}
	if !doctorProxyEnvHasPrefix(info.Env, "TAKO_PROXY_EMAIL=") {
		missing = append(missing, "TAKO_PROXY_EMAIL")
	}
	for _, port := range []string{"80/tcp", "443/tcp", "443/udp"} {
		if !doctorProxyPortExists(info.Ports, port) && !doctorProxyPortExists(info.HostPorts, port) {
			missing = append(missing, port+" publish")
		}
	}
	requiredMounts := map[string]string{
		"/etc/caddy":     "/etc/tako/proxy/caddy",
		"/data":          "/etc/tako/proxy/caddy-data",
		"/config":        "/etc/tako/proxy/caddy-config",
		"/var/log/caddy": "/var/log/tako/proxy",
	}
	for destination, source := range requiredMounts {
		if !doctorProxyMountExists(info.Mounts, source, destination) {
			missing = append(missing, destination+" mount")
		}
	}
	return missing
}

func doctorProxyPortExists(ports map[string]json.RawMessage, port string) bool {
	if ports == nil {
		return false
	}
	_, ok := ports[port]
	return ok
}

func doctorProxyArgExists(args []string, expected string) bool {
	for _, arg := range args {
		if arg == expected {
			return true
		}
	}
	return false
}

func doctorProxyEnvHasPrefix(env []string, prefix string) bool {
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func doctorProxyMountExists(mounts []doctorProxyMount, source string, destination string) bool {
	for _, mount := range mounts {
		if mount.Source == source && mount.Destination == destination {
			return true
		}
	}
	return false
}

type doctorReplicatedStateInfo struct {
	HasHistory          bool
	HistoryDeployments  int
	HasDesired          bool
	DesiredServices     int
	HasActual           bool
	ActualServices      int
	NodeActualSnapshots int
	Lease               *remotestate.LeaseInfo
}

func checkReplicatedState(record func(checkResult), cfg *config.Config, envName string, clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check replicated state", ""})
		return
	}

	envServerNames, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		record(checkResult{"WARN", fmt.Sprintf("Cannot resolve environment servers for replicated state: %v", err), ""})
		return
	}

	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)

	checkReplicatedStateWith(record, clientNames, func(clientName string) (*doctorReplicatedStateInfo, error) {
		return detectDoctorReplicatedState(clients[clientName], cfg, envName, envServerNames)
	})
}

func checkReplicatedStateWith(record func(checkResult), clientNames []string, probe func(string) (*doctorReplicatedStateInfo, error)) {
	for _, clientName := range clientNames {
		info, err := probe(clientName)
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot read replicated state: %v", clientName, err), "Run 'tako setup', then 'tako state repair' if the node should keep participating"})
			continue
		}
		if info == nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Cannot read replicated state: empty result", clientName), "Run 'tako setup', then 'tako state repair' if the node should keep participating"})
			continue
		}
		if !info.HasHistory && !info.HasDesired && !info.HasActual {
			record(checkResult{"WARN", fmt.Sprintf("%s: No replicated deployment or runtime state recorded", clientName), "Run 'tako deploy' for first deploy or 'tako state repair' to restore from another node"})
			continue
		}

		missing := doctorReplicatedStateMissing(info)
		if len(missing) > 0 {
			record(checkResult{"WARN", fmt.Sprintf("%s: Replicated state incomplete (missing %s)", clientName, strings.Join(missing, ", ")), "Run 'tako state repair' before the next deploy"})
		} else {
			record(checkResult{"PASS", fmt.Sprintf("%s: Replicated state present (history %d deployment(s), desired %d service(s), actual %d service(s), node actual %d snapshot(s))", clientName, info.HistoryDeployments, info.DesiredServices, info.ActualServices, info.NodeActualSnapshots), ""})
		}

		if info.Lease == nil {
			record(checkResult{"PASS", fmt.Sprintf("%s: Remote operation lease free", clientName), ""})
		} else {
			record(checkResult{"WARN", fmt.Sprintf("%s: Remote operation lease held by %s (%s)", clientName, stateLeaseWho(info.Lease), stateLeaseOperation(info.Lease)), "Inspect with 'tako state lease' and release only the exact stale ID if needed"})
		}
	}
}

func doctorReplicatedStateMissing(info *doctorReplicatedStateInfo) []string {
	if info == nil {
		return []string{"state"}
	}
	var missing []string
	if !info.HasHistory {
		missing = append(missing, "deployment history")
	}
	if !info.HasDesired {
		missing = append(missing, "desired runtime")
	}
	if !info.HasActual {
		missing = append(missing, "actual runtime")
	}
	if info.NodeActualSnapshots == 0 {
		missing = append(missing, "node actual snapshots")
	}
	return missing
}

func detectDoctorReplicatedState(client *ssh.Client, cfg *config.Config, envName string, envServerNames []string) (*doctorReplicatedStateInfo, error) {
	info := &doctorReplicatedStateInfo{}

	manager := remotestate.NewStateManagerWithSocket(client, cfg.Project.Name, envName, "", takodSocketFromConfig(cfg)).
		WithRequestTimeout(stateStatusRequestTimeout)
	history, err := manager.LoadHistory()
	if err != nil && !errors.Is(err, remotestate.ErrNotFound) {
		return nil, fmt.Errorf("deployment history: %w", err)
	}
	if historyHasDeployments(history) {
		info.HasHistory = true
		info.HistoryDeployments = deploymentHistoryCount(history)
	}

	runtime := takodstate.NewManager(client, cfg, envName).
		WithRequestTimeout(stateStatusRequestTimeout)
	desired, err := runtime.ReadDesired()
	if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
		return nil, fmt.Errorf("desired runtime: %w", err)
	}
	if desiredRevisionRepairable(desired) {
		info.HasDesired = true
		info.DesiredServices = len(desired.Services)
	}

	actual, err := runtime.ReadActual()
	if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
		return nil, fmt.Errorf("actual runtime: %w", err)
	}
	if actualSnapshotRepairable(actual) {
		info.HasActual = true
		info.ActualServices = actualSnapshotServiceCount(actual)
	}

	for _, nodeName := range envServerNames {
		nodeActual, err := runtime.ReadNodeActual(nodeName)
		if err != nil && !errors.Is(err, takodstate.ErrNotFound) {
			return nil, fmt.Errorf("node actual runtime for %s: %w", nodeName, err)
		}
		if nodeActualSnapshotRepairable(nodeActual, nodeName) {
			info.NodeActualSnapshots++
		}
	}

	lease, err := manager.ReadLease()
	if err != nil {
		return nil, fmt.Errorf("remote lease: %w", err)
	}
	info.Lease = lease
	return info, nil
}

func stateLeaseWho(lease *remotestate.LeaseInfo) string {
	if lease == nil || strings.TrimSpace(lease.Who) == "" {
		return "unknown"
	}
	return strings.TrimSpace(lease.Who)
}

func stateLeaseOperation(lease *remotestate.LeaseInfo) string {
	if lease == nil || strings.TrimSpace(lease.Operation) == "" {
		return "unknown operation"
	}
	return strings.TrimSpace(lease.Operation)
}

func checkConfig(record func(checkResult)) (*config.Config, error) {
	// Check for config file existence
	configName := cfgFile
	if configName != "" {
		if _, err := os.Stat(configName); err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("Config file: Not found: %s", configName), "Check --config path"})
			return nil, fmt.Errorf("no config")
		}
	} else {
		for _, name := range []string{"tako.yaml", "tako.yml", "tako.json"} {
			if _, err := os.Stat(name); err == nil {
				configName = name
				break
			}
		}
	}

	if configName == "" {
		record(checkResult{"FAIL", "Config file: Not found", "Create tako.yaml in project root"})
		return nil, fmt.Errorf("no config")
	}

	record(checkResult{"PASS", fmt.Sprintf("Config file: Found %s", configName), ""})

	// Try loading config
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		record(checkResult{"FAIL", fmt.Sprintf("Config parse: %v", err), configLoadFix(err)})
		return nil, err
	}

	record(checkResult{"PASS", fmt.Sprintf("Config parse: Valid (project: %s)", cfg.Project.Name), ""})
	return cfg, nil
}

func configLoadFix(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "failed to parse YAML") || strings.Contains(msg, "failed to parse JSON") {
		return "Fix syntax errors in config file"
	}
	if strings.Contains(msg, "missing environment variable") {
		return "Set the missing variable in .env or your shell"
	}
	if strings.Contains(msg, "host is required") {
		return "Set the configured host variable in .env or replace the host placeholder in config"
	}
	if strings.Contains(msg, "SSH key not found") {
		return "Check sshKey path, create the key, or configure password via an environment variable"
	}
	return "Fix config values or missing environment variables"
}

func checkEnvFile(record func(checkResult)) {
	if _, err := os.Stat(".env"); err == nil {
		record(checkResult{"PASS", ".env file: Found", ""})
		return
	}

	if _, err := os.Stat(".env.example"); err == nil {
		record(checkResult{"WARN", ".env file: Missing", "cp .env.example .env"})
		return
	}

	record(checkResult{"WARN", ".env file: Missing (no .env.example found either)", "Create .env with required variables"})
}

func checkSSHKeys(record func(checkResult), cfg *config.Config, envName string) {
	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		record(checkResult{"WARN", fmt.Sprintf("Cannot resolve servers: %v", err), ""})
		return
	}

	for _, serverName := range servers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Server not found in config", serverName), ""})
			continue
		}

		if server.Password != "" && server.SSHKey == "" {
			record(checkResult{"WARN", fmt.Sprintf("%s: Password auth configured", serverName), "Prefer sshKey; Tako setup hardening disables password SSH on managed hosts"})
			continue
		}

		keyPath := server.SSHKey
		if keyPath == "" {
			record(checkResult{"WARN", fmt.Sprintf("%s: No SSH key configured", serverName), "Add sshKey to server config"})
			continue
		}

		// Expand ~
		if strings.HasPrefix(keyPath, "~") {
			homeDir, _ := os.UserHomeDir()
			keyPath = filepath.Join(homeDir, keyPath[1:])
		}

		info, err := os.Stat(keyPath)
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s: SSH key not found: %s", serverName, keyPath), "Check key path or copy key to this machine"})
			continue
		}

		// Check permissions (should be 0600)
		perm := info.Mode().Perm()
		if perm&0077 != 0 {
			record(checkResult{"WARN", fmt.Sprintf("%s: %s (%04o — too open)", serverName, keyPath, perm), fmt.Sprintf("chmod 600 %s", keyPath)})
		} else {
			record(checkResult{"PASS", fmt.Sprintf("%s: %s (%04o)", serverName, keyPath, perm), ""})
		}
	}
}

func checkSecrets(record func(checkResult), cfg *config.Config, envName string) {
	// Check if secrets files exist
	secretsBase := ".tako/secrets"
	secretsEnv := fmt.Sprintf(".tako/secrets.%s", envName)

	baseExists := false
	envExists := false
	if _, err := os.Stat(secretsBase); err == nil {
		baseExists = true
	}
	if _, err := os.Stat(secretsEnv); err == nil {
		envExists = true
	}

	if !baseExists && !envExists {
		// Check if any services use secrets
		services, err := cfg.GetServices(envName)
		if err == nil {
			needsSecrets := false
			for _, svc := range services {
				if len(svc.Secrets) > 0 {
					needsSecrets = true
					break
				}
			}
			if needsSecrets {
				record(checkResult{"WARN", "Secrets files: Missing (services require secrets)", "Run 'tako secrets set KEY=VALUE'"})
			} else {
				record(checkResult{"PASS", "Secrets: No secrets required by services", ""})
			}
		}
		return
	}

	if baseExists {
		record(checkResult{"PASS", fmt.Sprintf("Secrets file: %s", secretsBase), ""})
	}
	if envExists {
		record(checkResult{"PASS", fmt.Sprintf("Secrets file: %s", secretsEnv), ""})
	}

	// Validate required secrets
	mgr, err := secrets.NewManager(envName)
	if err != nil {
		record(checkResult{"WARN", fmt.Sprintf("Secrets manager: %v", err), ""})
		return
	}

	// Collect required secrets from services
	services, err := cfg.GetServices(envName)
	if err != nil {
		return
	}
	var required []string
	for _, svc := range services {
		required = append(required, svc.Secrets...)
	}

	if len(required) > 0 {
		if err := mgr.ValidateRequired(required); err != nil {
			record(checkResult{"WARN", fmt.Sprintf("Required secrets: %v", err), "Run 'tako secrets set' for missing secrets"})
		} else {
			record(checkResult{"PASS", fmt.Sprintf("Required secrets: All %d present", len(required)), ""})
		}
	}
}

func checkLocalState(record func(checkResult)) {
	if _, err := os.Stat(".tako"); err != nil {
		record(checkResult{"WARN", ".tako directory: Missing", "Run 'tako state pull' or 'tako deploy'"})
	} else {
		record(checkResult{"PASS", ".tako directory: Found", ""})
	}

	if _, err := os.Stat(".gitignore"); err == nil {
		data, _ := os.ReadFile(".gitignore")
		if strings.Contains(string(data), ".tako") {
			record(checkResult{"PASS", ".gitignore: Contains .tako", ""})
		} else {
			record(checkResult{"WARN", ".gitignore: Missing .tako entry", "Add .tako/ to .gitignore"})
		}
	} else {
		record(checkResult{"WARN", ".gitignore: Not found", "Create .gitignore with .tako/ entry"})
	}
}

func checkServerConnectivity(record func(checkResult), cfg *config.Config, envName string) map[string]*ssh.Client {
	clients := make(map[string]*ssh.Client)

	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		record(checkResult{"FAIL", fmt.Sprintf("Cannot resolve servers: %v", err), ""})
		return clients
	}

	for _, result := range collectServerConnectivity(cfg.Servers, servers, func(serverName string, server config.ServerConfig) doctorConnectivityResult {
		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     server.Host,
			Port:     server.Port,
			User:     server.User,
			SSHKey:   server.SSHKey,
			Password: server.Password,
		})
		if err != nil {
			return doctorConnectivityResult{
				serverName: serverName,
				result:     checkResult{"FAIL", fmt.Sprintf("%s (%s): %v", serverName, server.Host, err), "Check SSH key and server config"},
			}
		}

		if err := client.Connect(); err != nil {
			return doctorConnectivityResult{
				serverName: serverName,
				result:     checkResult{"FAIL", fmt.Sprintf("%s (%s): Connection failed — %v", serverName, server.Host, err), "Check server is running and SSH access is configured"},
			}
		}

		// Verify connectivity with a simple command
		output, err := client.Execute("echo 'tako-ok'")
		if err != nil {
			client.Close()
			return doctorConnectivityResult{
				serverName: serverName,
				result:     checkResult{"FAIL", fmt.Sprintf("%s (%s): Connected but command failed — %v", serverName, server.Host, err), ""},
			}
		}

		if strings.TrimSpace(output) != "tako-ok" {
			return doctorConnectivityResult{
				serverName: serverName,
				client:     client,
				result:     checkResult{"WARN", fmt.Sprintf("%s (%s): Connected but unexpected output", serverName, server.Host), ""},
			}
		}
		return doctorConnectivityResult{
			serverName: serverName,
			client:     client,
			result:     checkResult{"PASS", fmt.Sprintf("%s (%s): Connected", serverName, server.Host), ""},
		}
	}) {
		record(result.result)
		if result.client != nil {
			clients[result.serverName] = result.client
		}
	}

	return clients
}

type doctorConnectivityFunc func(serverName string, server config.ServerConfig) doctorConnectivityResult

type doctorConnectivityResult struct {
	index      int
	serverName string
	client     *ssh.Client
	result     checkResult
}

func collectServerConnectivity(servers map[string]config.ServerConfig, serverNames []string, connect doctorConnectivityFunc) []doctorConnectivityResult {
	resultCh := make(chan doctorConnectivityResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		server, ok := servers[serverName]
		if !ok {
			resultCh <- doctorConnectivityResult{
				index:      index,
				serverName: serverName,
				result:     checkResult{"FAIL", fmt.Sprintf("%s: Not found in config", serverName), ""},
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, server config.ServerConfig) {
			defer wg.Done()
			result := connect(serverName, server)
			result.index = index
			if result.serverName == "" {
				result.serverName = serverName
			}
			resultCh <- result
		}(index, serverName, server)
	}

	wg.Wait()
	close(resultCh)

	results := make([]doctorConnectivityResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}
	return results
}

func checkRunningServices(record func(checkResult), cfg *config.Config, envName string, clients map[string]*ssh.Client) {
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check", ""})
		return
	}

	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)

	totalNodes := 0
	totalServices := 0
	totalReplicas := 0
	for _, clientName := range clientNames {
		client := clients[clientName]
		actual, err := actualStateViaTakod(client, cfg, envName)
		if err != nil {
			record(checkResult{"WARN", fmt.Sprintf("Cannot read takod actual state on %s: %v", clientName, err), "Run 'tako setup' to install/start takod"})
			continue
		}
		if len(actual.Services) == 0 {
			record(checkResult{"WARN", fmt.Sprintf("No running services found on %s", clientName), "Run 'tako deploy' to start services"})
			continue
		}

		totalNodes++
		totalServices += len(actual.Services)
		nodeReplicas := 0
		for _, service := range actual.Services {
			if service != nil {
				nodeReplicas += service.Replicas
			}
		}
		totalReplicas += nodeReplicas
		record(checkResult{"PASS", fmt.Sprintf("takod services on %s: %d service(s), %d replica(s)", clientName, len(actual.Services), nodeReplicas), ""})
		names := make([]string, 0, len(actual.Services))
		for name := range actual.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			service := actual.Services[name]
			if service == nil {
				continue
			}
			record(checkResult{"PASS", fmt.Sprintf("  %s: %d replica(s)", name, service.Replicas), ""})
		}
	}
	if totalNodes > 1 {
		record(checkResult{"PASS", fmt.Sprintf("takod mesh services: %d node(s), %d node-local service(s), %d replica(s)", totalNodes, totalServices, totalReplicas), ""})
	}
}

func checkExternalVolumes(record func(checkResult), cfg *config.Config, envName string, clients map[string]*ssh.Client) {
	volumes := externalVolumeNamesForEnvironment(cfg, envName)
	if len(volumes) == 0 {
		record(checkResult{"PASS", "No external volumes configured", ""})
		return
	}
	if len(clients) == 0 {
		record(checkResult{"SKIP", "No connected servers to check external volumes", ""})
		return
	}

	clientNames := make([]string, 0, len(clients))
	for name := range clients {
		clientNames = append(clientNames, name)
	}
	sort.Strings(clientNames)

	for _, clientName := range clientNames {
		client := clients[clientName]
		for _, volume := range volumes {
			if _, err := client.Execute("docker volume inspect " + utils.ShellQuote(volume) + " >/dev/null"); err != nil {
				record(checkResult{"FAIL", fmt.Sprintf("%s: external volume %s missing", clientName, volume), "Create or import the Docker volume before deploy"})
				continue
			}
			record(checkResult{"PASS", fmt.Sprintf("%s: external volume %s present", clientName, volume), ""})
		}
	}
}
