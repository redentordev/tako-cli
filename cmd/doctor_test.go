package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/health"
	"github.com/redentordev/tako-cli/pkg/mesh"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

func TestCheckProxyClientIPTopologyWarnsForWrongOrProxiedDNS(t *testing.T) {
	specs := []engine.DomainStatusSpec{
		{Service: "wrong", Domain: "wrong.example.com", WarnUntrustedAccessControls: true},
		{Service: "proxied", Domain: "proxied.example.com", WarnUntrustedAccessControls: true},
		{Service: "trusted", Domain: "trusted.example.com"},
	}
	var results []checkResult
	checkProxyClientIPTopologyWith(func(result checkResult) {
		results = append(results, result)
	}, specs, []string{"203.0.113.10"}, func(spec engine.DomainStatusSpec) health.DomainStatus {
		if spec.Service == "wrong" {
			return health.DomainStatus{DNS: health.DomainDNSWrong}
		}
		return health.DomainStatus{DNS: health.DomainDNSProxied}
	})
	if len(results) != 2 || results[0].status != "WARN" || results[1].status != "WARN" {
		t.Fatalf("results = %#v, want two warnings", results)
	}
}

func TestCheckACMEDNSCertificatesFlagsFailuresAndOrphans(t *testing.T) {
	now := time.Now().UTC()
	retry := now.Add(time.Hour)
	var results []checkResult
	checkACMEDNSCertificatesWith(func(result checkResult) { results = append(results, result) }, true, "demo", "production", []string{"node-a"}, func(string) ([]takod.ProxyCertificateMetadata, error) {
		return []takod.ProxyCertificateMetadata{
			{Domain: "healthy.example.com", Source: takod.CertificateSourceACMEDNS, OwnerProject: "demo", OwnerEnvironment: "production", NotAfter: now.Add(90 * 24 * time.Hour)},
			{Domain: "failed.example.com", Source: takod.CertificateSourceACMEDNS, OwnerProject: "demo", OwnerEnvironment: "production", NotAfter: now.Add(20 * 24 * time.Hour), LastError: "bad token", RetryAfter: &retry},
			{Domain: "*.old.example.com", Source: takod.CertificateSourceACMEDNS, OwnerProject: "demo", OwnerEnvironment: "production", NotAfter: now.Add(60 * 24 * time.Hour), Orphaned: true},
			{Domain: "unrelated.example.net", Source: takod.CertificateSourceACMEDNS, OwnerProject: "other", OwnerEnvironment: "production", LastError: "must not be reported"},
		}, nil
	})
	if len(results) != 3 || results[0].status != "PASS" || results[1].status != "WARN" || results[2].status != "WARN" {
		t.Fatalf("results = %#v", results)
	}
	if !strings.Contains(results[1].message, "retry after") || !strings.Contains(results[2].message, "will not renew") {
		t.Fatalf("results = %#v", results)
	}
}

func TestCheckACMEDNSCertificatesSkipsWhenEnvironmentOwnsNone(t *testing.T) {
	var results []checkResult
	checkACMEDNSCertificatesWith(func(result checkResult) { results = append(results, result) }, false, "demo", "production", []string{"node-a"}, func(string) ([]takod.ProxyCertificateMetadata, error) {
		t.Fatal("probe should not run")
		return nil, nil
	})
	if len(results) != 1 || results[0].status != "SKIP" {
		t.Fatalf("results = %#v", results)
	}
}

func TestRunDoctorMachineOutputMissingConfig(t *testing.T) {
	switchToTempDir(t)
	oldCfgFile, oldSkipRemote, oldOutput := cfgFile, doctorSkipRemote, outputFormatFlag
	cfgFile, doctorSkipRemote, outputFormatFlag = "", true, outputFormatJSON
	t.Cleanup(func() { cfgFile, doctorSkipRemote, outputFormatFlag = oldCfgFile, oldSkipRemote, oldOutput })

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runDoctor(&cobra.Command{}, nil)
	})
	if runErr == nil {
		t.Fatal("runDoctor should surface failed checks")
	}
	if engine.Classify(runErr) != engine.ClassAttention {
		t.Fatalf("doctor failure classified as %d, want ClassAttention", engine.Classify(runErr))
	}

	var result engine.DoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Kind != engine.KindDoctorResult || result.Status != "attention" {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if result.Failed == 0 || len(result.Checks) == 0 {
		t.Fatalf("missing config should record a failed check: %+v", result)
	}
	if result.Checks[0].Name != "Configuration" || result.Checks[0].Status != engine.DoctorStatusFail {
		t.Fatalf("first check = %+v", result.Checks[0])
	}
}

func TestRunDoctorSkipRemoteRecordsSkippedChecks(t *testing.T) {
	root := switchToTempDir(t)
	sshKey := filepath.Join(root, "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatalf("failed to write ssh key fixture: %v", err)
	}
	configData := []byte(`project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 203.0.113.10
    user: deploy
    sshKey: ` + sshKey + `
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
`)
	if err := os.WriteFile(filepath.Join(root, "tako.yaml"), configData, 0600); err != nil {
		t.Fatalf("failed to write valid config: %v", err)
	}
	oldCfgFile, oldEnvFlag, oldSkipRemote, oldOutput := cfgFile, envFlag, doctorSkipRemote, outputFormatFlag
	cfgFile, envFlag, doctorSkipRemote, outputFormatFlag = "", "", true, outputFormatJSON
	t.Cleanup(func() {
		cfgFile, envFlag, doctorSkipRemote, outputFormatFlag = oldCfgFile, oldEnvFlag, oldSkipRemote, oldOutput
	})

	var runErr error
	stdout := captureStdout(t, func() {
		runErr = runDoctor(&cobra.Command{}, nil)
	})
	if runErr != nil {
		t.Fatalf("runDoctor returned error: %v", runErr)
	}

	var result engine.DoctorResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("stdout is not a single JSON document: %v\n%s", err, stdout)
	}
	if result.Status != "ok" || !result.SkipRemote {
		t.Fatalf("unexpected result document: %+v", result)
	}
	if result.Project != "demo" || result.Environment != "production" {
		t.Fatalf("identity fields wrong: %+v", result)
	}
	skips := 0
	for _, check := range result.Checks {
		if check.Status == engine.DoctorStatusSkip {
			skips++
		}
	}
	if skips < 7 {
		t.Fatalf("expected the 7 remote sections to record skip checks, got %d: %+v", skips, result.Checks)
	}
}

func TestCheckConfigHonorsConfigFlag(t *testing.T) {
	tempDir := t.TempDir()
	keyPath := filepath.Join(tempDir, "id_ed25519")
	if err := os.WriteFile(keyPath, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(tempDir, "custom-tako.yaml")
	if err := os.WriteFile(configPath, []byte(`
project:
  name: demo
  version: 1.0.0
servers:
  node-a:
    host: 127.0.0.1
    user: root
    sshKey: `+keyPath+`
environments:
  production:
    servers: [node-a]
    services:
      web:
        image: nginx:alpine
        port: 80
`), 0644); err != nil {
		t.Fatal(err)
	}

	oldCfgFile := cfgFile
	cfgFile = configPath
	t.Cleanup(func() {
		cfgFile = oldCfgFile
	})

	var results []checkResult
	cfg, err := checkConfig(func(result checkResult) {
		results = append(results, result)
	})
	if err != nil {
		t.Fatalf("checkConfig returned error: %v", err)
	}
	if cfg.Project.Name != "demo" {
		t.Fatalf("project name = %q, want demo", cfg.Project.Name)
	}
	if len(results) < 1 || results[0].status != "PASS" || !strings.Contains(results[0].message, configPath) {
		t.Fatalf("first result = %#v, want config flag path pass", results)
	}
}

func TestCheckSSHKeysWarnsOnPasswordOnlyAuth(t *testing.T) {
	cfg := &config.Config{
		Servers: map[string]config.ServerConfig{
			"prod": {
				Host:     "example.com",
				User:     "deploy",
				Password: "${SSH_PASSWORD}",
			},
		},
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Servers: []string{"prod"},
			},
		},
	}

	var results []checkResult
	checkSSHKeys(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production")

	if len(results) != 1 {
		t.Fatalf("got %d result(s), want 1: %#v", len(results), results)
	}
	if results[0].status != "WARN" {
		t.Fatalf("password-only auth status = %q, want WARN", results[0].status)
	}
	if !strings.Contains(results[0].message, "Password auth configured") {
		t.Fatalf("unexpected warning message: %q", results[0].message)
	}
	if !strings.Contains(results[0].fix, "Prefer sshKey") {
		t.Fatalf("unexpected warning fix: %q", results[0].fix)
	}
}

func TestConfigLoadFixDistinguishesParseAndValidationErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "yaml parse",
			err:  errString("failed to parse YAML config: yaml: line 1"),
			want: "Fix syntax errors",
		},
		{
			name: "missing host",
			err:  errString("invalid config: server production: host is required"),
			want: "host variable",
		},
		{
			name: "missing env",
			err:  errString("failed to expand config environment variables: missing environment variable(s): SERVER_HOST"),
			want: "missing variable",
		},
		{
			name: "ssh key",
			err:  errString("invalid config: server production: SSH key not found"),
			want: "sshKey",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := configLoadFix(tt.err); !strings.Contains(got, tt.want) {
				t.Fatalf("configLoadFix() = %q, want containing %q", got, tt.want)
			}
		})
	}
}

func TestCollectServerConnectivityRunsConcurrentlyAndKeepsOrder(t *testing.T) {
	serverNames := []string{"node-a", "node-b", "node-c"}
	servers := testDoctorServers(serverNames)
	started := make(chan string, len(serverNames))
	release := make(chan struct{})

	resultsDone := make(chan []doctorConnectivityResult, 1)
	go func() {
		resultsDone <- collectServerConnectivity(servers, serverNames, func(serverName string, _ config.ServerConfig) doctorConnectivityResult {
			started <- serverName
			<-release
			return doctorConnectivityResult{
				serverName: serverName,
				client:     &ssh.Client{},
				result:     checkResult{"PASS", serverName + " connected", ""},
			}
		})
	}()

	waitForDoctorConnectivityStarts(t, started, len(serverNames))
	close(release)

	results := <-resultsDone
	if len(results) != len(serverNames) {
		t.Fatalf("results = %d, want %d", len(results), len(serverNames))
	}
	for i, serverName := range serverNames {
		if results[i].serverName != serverName {
			t.Fatalf("result %d server = %q, want %q", i, results[i].serverName, serverName)
		}
		if results[i].client == nil {
			t.Fatalf("result %d client is nil", i)
		}
		if results[i].result.message != serverName+" connected" {
			t.Fatalf("result %d message = %q", i, results[i].result.message)
		}
	}
}

type errString string

func (e errString) Error() string {
	return string(e)
}

func TestCollectServerConnectivityReportsMissingServerInOrder(t *testing.T) {
	results := collectServerConnectivity(
		map[string]config.ServerConfig{"node-a": {Host: "node-a.example.test"}},
		[]string{"node-a", "node-b"},
		func(serverName string, _ config.ServerConfig) doctorConnectivityResult {
			return doctorConnectivityResult{
				serverName: serverName,
				result:     checkResult{"PASS", serverName + " connected", ""},
			}
		},
	)

	if results[0].result.status != "PASS" {
		t.Fatalf("node-a status = %q, want PASS", results[0].result.status)
	}
	if results[1].serverName != "node-b" || results[1].result.status != "FAIL" {
		t.Fatalf("node-b result = %#v, want missing-server failure", results[1])
	}
	if !strings.Contains(results[1].result.message, "Not found in config") {
		t.Fatalf("node-b message = %q", results[1].result.message)
	}
}

func TestCheckDockerRuntimeWithReportsRootfulPass(t *testing.T) {
	var results []checkResult
	checkDockerRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*provisioner.DockerRuntimeInfo, error) {
		return &provisioner.DockerRuntimeInfo{ServerVersion: "29.1.3", RootDir: "/var/lib/docker"}, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "Docker rootful daemon 29.1.3") {
		t.Fatalf("result = %#v, want rootful pass", results[0])
	}
}

func TestCheckDockerRuntimeWithFailsRootless(t *testing.T) {
	var results []checkResult
	checkDockerRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*provisioner.DockerRuntimeInfo, error) {
		return &provisioner.DockerRuntimeInfo{Rootless: true}, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "Docker runtime is rootless") {
		t.Fatalf("result = %#v, want rootless failure", results[0])
	}
	if !strings.Contains(results[0].fix, "rootful system Docker") {
		t.Fatalf("fix = %q, want rootful guidance", results[0].fix)
	}
}

func TestCheckDockerRuntimeWithFailsProbeErrors(t *testing.T) {
	var results []checkResult
	checkDockerRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*provisioner.DockerRuntimeInfo, error) {
		return nil, errors.New("daemon unavailable")
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "Docker runtime unsupported") || !strings.Contains(results[0].message, "daemon unavailable") {
		t.Fatalf("result = %#v, want probe failure", results[0])
	}
}

func TestCheckServerAgentVersionWithReportsMatchingAgent(t *testing.T) {
	var results []checkResult
	checkServerAgentVersionWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, "v0.4.39", func(string) (*takodRemoteStatus, error) {
		return &takodRemoteStatus{Runtime: "takod", Version: "v0.4.39", Hostname: "host-a"}, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "matches CLI v0.4.39") {
		t.Fatalf("result = %#v, want matching-agent pass", results[0])
	}
}

func TestCheckServerAgentVersionWithFailsStaleReleasedAgent(t *testing.T) {
	var results []checkResult
	checkServerAgentVersionWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, "v0.4.39", func(string) (*takodRemoteStatus, error) {
		return &takodRemoteStatus{Runtime: "takod", Version: "v0.4.38", Hostname: "host-a"}, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "differs from CLI v0.4.39") {
		t.Fatalf("result = %#v, want stale-agent failure", results[0])
	}
	if !strings.Contains(results[0].fix, "tako upgrade servers") {
		t.Fatalf("fix = %q, want upgrade guidance", results[0].fix)
	}
}

func TestCheckServerAgentVersionWithWarnsForDevelopmentCLI(t *testing.T) {
	var results []checkResult
	checkServerAgentVersionWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, "dev", func(string) (*takodRemoteStatus, error) {
		return &takodRemoteStatus{Runtime: "takod", Version: "v0.4.39", Hostname: "host-a"}, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "WARN" || !strings.Contains(results[0].message, "development CLI dev") {
		t.Fatalf("result = %#v, want development-cli warning", results[0])
	}
	if !strings.Contains(results[0].fix, "--takod-binary") {
		t.Fatalf("fix = %q, want takod-binary guidance", results[0].fix)
	}
}

func TestCheckServerAgentVersionWithFailsProbeErrors(t *testing.T) {
	var results []checkResult
	checkServerAgentVersionWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, "v0.4.39", func(string) (*takodRemoteStatus, error) {
		return nil, errors.New("status unavailable")
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "status unavailable") {
		t.Fatalf("result = %#v, want status failure", results[0])
	}
}

func TestPublicServiceNamesReturnsSortedProxyServices(t *testing.T) {
	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"worker": {},
					"api":    {Port: 3000},
					"web":    {Port: 3000, Proxy: &config.ProxyConfig{Domain: "web.example.com"}},
					"admin":  {Port: 3000, Proxy: &config.ProxyConfig{Domain: "admin.example.com"}},
				},
			},
		},
	}

	got, err := publicServiceNames(cfg, "production")
	if err != nil {
		t.Fatalf("publicServiceNames returned error: %v", err)
	}
	if strings.Join(got, ",") != "admin,web" {
		t.Fatalf("public services = %#v, want sorted admin/web", got)
	}
}

func TestCheckProxyRuntimeWithReportsReadyProxy(t *testing.T) {
	var results []checkResult
	checkProxyRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, "node-a", []string{"web"}, func() (*doctorProxyRuntimeInfo, error) {
		return testDoctorProxyRuntimeInfo(), nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "HTTP/3 UDP 443") {
		t.Fatalf("result = %#v, want proxy readiness pass", results[0])
	}
}

func TestCheckProxyRuntimeWithUsesHostPortBindingsFallback(t *testing.T) {
	info := testDoctorProxyRuntimeInfo()
	info.Ports = map[string]json.RawMessage{}
	info.HostPorts = map[string]json.RawMessage{
		"80/tcp":  json.RawMessage(`[{"HostPort":"80"}]`),
		"443/tcp": json.RawMessage(`[{"HostPort":"443"}]`),
		"443/udp": json.RawMessage(`[{"HostPort":"443"}]`),
	}

	var results []checkResult
	checkProxyRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, "node-a", []string{"web"}, func() (*doctorProxyRuntimeInfo, error) {
		return info, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "PASS" {
		t.Fatalf("result = %#v, want proxy readiness pass from host port bindings", results[0])
	}
}

func TestCheckProxyRuntimeWithWarnsWhenProxyMissing(t *testing.T) {
	var results []checkResult
	checkProxyRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, "node-a", []string{"admin", "web"}, func() (*doctorProxyRuntimeInfo, error) {
		return nil, errDoctorProxyMissing
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "WARN" || !strings.Contains(results[0].message, "admin, web") {
		t.Fatalf("result = %#v, want missing proxy warning with public services", results[0])
	}
	if !strings.Contains(results[0].fix, "tako deploy") {
		t.Fatalf("fix = %q, want deploy guidance", results[0].fix)
	}
}

func TestCheckProxyRuntimeWithFailsStaleProxy(t *testing.T) {
	stale := testDoctorProxyRuntimeInfo()
	delete(stale.Ports, "443/udp")

	var results []checkResult
	checkProxyRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, "node-a", []string{"web"}, func() (*doctorProxyRuntimeInfo, error) {
		return stale, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "443/udp publish") {
		t.Fatalf("result = %#v, want stale proxy failure", results[0])
	}
}

func TestCheckProxyRuntimeWithFailsStoppedProxy(t *testing.T) {
	stopped := testDoctorProxyRuntimeInfo()
	stopped.Running = false

	var results []checkResult
	checkProxyRuntimeWith(func(result checkResult) {
		results = append(results, result)
	}, "node-a", []string{"web"}, func() (*doctorProxyRuntimeInfo, error) {
		return stopped, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "exists but is not running") {
		t.Fatalf("result = %#v, want stopped proxy failure", results[0])
	}
}

func TestParseDoctorProxyRuntime(t *testing.T) {
	info, err := parseDoctorProxyRuntime(`true
["run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile","--watch"]
{"443/udp":[{"HostPort":"443"}]}
[{"Source":"/etc/tako/proxy/caddy","Destination":"/etc/caddy"}]
["TAKO_PROXY_EMAIL=ops@example.com"]
caddy:2.9-alpine`)
	if err != nil {
		t.Fatalf("parseDoctorProxyRuntime returned error: %v", err)
	}
	if !info.Running {
		t.Fatal("expected running state")
	}
	if info.Image != "caddy:2.9-alpine" {
		t.Fatalf("image = %q, want caddy", info.Image)
	}
	if _, ok := info.Ports["443/udp"]; !ok {
		t.Fatalf("ports = %#v, want UDP 443", info.Ports)
	}
}

func TestParseDoctorProxyRuntimeReadsHostPortBindings(t *testing.T) {
	info, err := parseDoctorProxyRuntime(`true
["run","--config","/etc/caddy/Caddyfile","--adapter","caddyfile","--watch"]
{}
{"443/udp":[{"HostPort":"443"}]}
[{"Source":"/etc/tako/proxy/caddy","Destination":"/etc/caddy"}]
["TAKO_PROXY_EMAIL=ops@example.com"]
caddy:2.9-alpine`)
	if err != nil {
		t.Fatalf("parseDoctorProxyRuntime returned error: %v", err)
	}
	if _, ok := info.HostPorts["443/udp"]; !ok {
		t.Fatalf("host ports = %#v, want UDP 443", info.HostPorts)
	}
}

func TestParseDoctorProxyRuntimeRejectsInvalidOutput(t *testing.T) {
	if _, err := parseDoctorProxyRuntime("true\n[]"); err == nil {
		t.Fatal("expected short inspect output to fail")
	}
	if _, err := parseDoctorProxyRuntime("not-json\n[]\n{}\n[]\n[]\ncaddy:2.9-alpine"); err == nil {
		t.Fatal("expected invalid running state to fail")
	}
}

func TestDetectDoctorProxyRuntimeUsesSudoDockerInspect(t *testing.T) {
	executor := &fakeDoctorExecutor{
		output: `true
["run","--config","/etc/caddy/Caddyfile"]
{}
[]
[]
caddy:2.9-alpine`,
	}
	if _, err := detectDoctorProxyRuntime(executor); err != nil {
		t.Fatalf("detectDoctorProxyRuntime returned error: %v", err)
	}
	if !strings.Contains(executor.command, "sudo docker inspect tako-proxy") {
		t.Fatalf("command = %q, want sudo docker inspect", executor.command)
	}
	if !strings.Contains(executor.command, "{{json .NetworkSettings.Ports}}") {
		t.Fatalf("command = %q, want ports inspect", executor.command)
	}
	if !strings.Contains(executor.command, "{{json .HostConfig.PortBindings}}") {
		t.Fatalf("command = %q, want host port binding inspect", executor.command)
	}
}

func TestDetectDoctorProxyRuntimeMapsMissingContainer(t *testing.T) {
	executor := &fakeDoctorExecutor{
		output: "Error: No such object: tako-proxy",
		err:    errors.New("exit status 1"),
	}
	_, err := detectDoctorProxyRuntime(executor)
	if !errors.Is(err, errDoctorProxyMissing) {
		t.Fatalf("error = %v, want errDoctorProxyMissing", err)
	}
}

func TestCheckReplicatedStateWithReportsCompleteState(t *testing.T) {
	var results []checkResult
	checkReplicatedStateWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*doctorReplicatedStateInfo, error) {
		return &doctorReplicatedStateInfo{
			HasHistory:          true,
			HistoryDeployments:  3,
			HasDesired:          true,
			DesiredServices:     2,
			HasActual:           true,
			ActualServices:      2,
			NodeActualSnapshots: 1,
		}, nil
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want state pass and lease pass", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "history 3 deployment(s)") {
		t.Fatalf("state result = %#v, want replicated state pass", results[0])
	}
	if results[1].status != "PASS" || !strings.Contains(results[1].message, "lease free") {
		t.Fatalf("lease result = %#v, want lease free pass", results[1])
	}
}

func TestCheckReplicatedStateWithWarnsWhenNoStateRecorded(t *testing.T) {
	var results []checkResult
	checkReplicatedStateWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*doctorReplicatedStateInfo, error) {
		return &doctorReplicatedStateInfo{}, nil
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "WARN" || !strings.Contains(results[0].message, "No replicated deployment or runtime state") {
		t.Fatalf("result = %#v, want no-state warning", results[0])
	}
	if !strings.Contains(results[0].fix, "tako deploy") || !strings.Contains(results[0].fix, "tako state repair") {
		t.Fatalf("fix = %q, want deploy/repair guidance", results[0].fix)
	}
}

func TestCheckReplicatedStateWithWarnsWhenStateIncomplete(t *testing.T) {
	var results []checkResult
	checkReplicatedStateWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*doctorReplicatedStateInfo, error) {
		return &doctorReplicatedStateInfo{
			HasHistory:         true,
			HistoryDeployments: 1,
			HasDesired:         true,
			DesiredServices:    1,
		}, nil
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want state warning and lease pass", results)
	}
	if results[0].status != "WARN" || !strings.Contains(results[0].message, "actual runtime") || !strings.Contains(results[0].message, "node actual snapshots") {
		t.Fatalf("state result = %#v, want incomplete-state warning", results[0])
	}
	if !strings.Contains(results[0].fix, "tako state repair") {
		t.Fatalf("fix = %q, want state repair guidance", results[0].fix)
	}
}

func TestCheckReplicatedStateWithWarnsOnHeldLease(t *testing.T) {
	var results []checkResult
	checkReplicatedStateWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*doctorReplicatedStateInfo, error) {
		return &doctorReplicatedStateInfo{
			HasHistory:          true,
			HistoryDeployments:  1,
			HasDesired:          true,
			DesiredServices:     1,
			HasActual:           true,
			ActualServices:      1,
			NodeActualSnapshots: 1,
			Lease:               &remotestate.LeaseInfo{Who: "ci", Operation: "deploy"},
		}, nil
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want state pass and lease warning", results)
	}
	if results[1].status != "WARN" || !strings.Contains(results[1].message, "held by ci (deploy)") {
		t.Fatalf("lease result = %#v, want held-lease warning", results[1])
	}
	if !strings.Contains(results[1].fix, "tako state lease") {
		t.Fatalf("fix = %q, want lease inspection guidance", results[1].fix)
	}
}

func TestCheckReplicatedStateWithFailsProbeErrors(t *testing.T) {
	var results []checkResult
	checkReplicatedStateWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, func(string) (*doctorReplicatedStateInfo, error) {
		return nil, errors.New("state endpoint unavailable")
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "state endpoint unavailable") {
		t.Fatalf("result = %#v, want replicated-state failure", results[0])
	}
}

func TestStateLeaseFallbackLabels(t *testing.T) {
	if got := stateLeaseWho(nil); got != "unknown" {
		t.Fatalf("stateLeaseWho(nil) = %q", got)
	}
	if got := stateLeaseOperation(&remotestate.LeaseInfo{}); got != "unknown operation" {
		t.Fatalf("stateLeaseOperation(empty) = %q", got)
	}
}

func TestCheckLocalBuildInputsWithReportsNoBuildServices(t *testing.T) {
	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {Image: "nginx:alpine"},
				},
			},
		},
	}

	var results []checkResult
	checkLocalBuildInputsWith(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production", func(string) bool {
		t.Fatal("nixpacks probe should not run for image-only services")
		return false
	})

	if len(results) != 1 {
		t.Fatalf("results = %#v, want one", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "No build-backed services") {
		t.Fatalf("result = %#v, want no-build pass", results[0])
	}
}

func TestCheckLocalBuildInputsWithReportsDockerfileBuild(t *testing.T) {
	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "Dockerfile"), []byte("FROM scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {Build: buildDir},
				},
			},
		},
	}

	var results []checkResult
	checkLocalBuildInputsWith(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production", func(string) bool {
		t.Fatal("nixpacks probe should not run when a Dockerfile exists")
		return false
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want remote-build note and service pass", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "local Docker daemon is not required") {
		t.Fatalf("note result = %#v, want remote-build note", results[0])
	}
	if results[1].status != "PASS" || !strings.Contains(results[1].message, "uses Dockerfile Dockerfile") {
		t.Fatalf("service result = %#v, want Dockerfile pass", results[1])
	}
}

func TestCheckLocalBuildInputsWithFailsWhenNixpacksMissing(t *testing.T) {
	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "package.json"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {Build: buildDir},
				},
			},
		},
	}

	var results []checkResult
	checkLocalBuildInputsWith(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production", func(string) bool {
		return false
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want remote-build note and service failure", results)
	}
	if results[1].status != "FAIL" || !strings.Contains(results[1].message, "Nixpacks is not installed") {
		t.Fatalf("service result = %#v, want missing-Nixpacks failure", results[1])
	}
	if !strings.Contains(results[1].fix, "Install Nixpacks") {
		t.Fatalf("fix = %q, want Nixpacks guidance", results[1].fix)
	}
}

func TestCheckLocalBuildInputsWithReportsNixpacksReady(t *testing.T) {
	buildDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(buildDir, "go.mod"), []byte("module example.test/app\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"api": {Build: buildDir},
				},
			},
		},
	}

	var results []checkResult
	checkLocalBuildInputsWith(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production", func(string) bool {
		return true
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want remote-build note and service pass", results)
	}
	if results[1].status != "PASS" || !strings.Contains(results[1].message, "go framework inputs") || !strings.Contains(results[1].message, "Nixpacks available") {
		t.Fatalf("service result = %#v, want Nixpacks-ready pass", results[1])
	}
}

func TestCheckLocalBuildInputsWithFailsUnknownFramework(t *testing.T) {
	buildDir := t.TempDir()
	cfg := &config.Config{
		Environments: map[string]config.EnvironmentConfig{
			"production": {
				Services: map[string]config.ServiceConfig{
					"web": {Build: buildDir},
				},
			},
		},
	}

	var results []checkResult
	checkLocalBuildInputsWith(func(result checkResult) {
		results = append(results, result)
	}, cfg, "production", func(string) bool {
		return true
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want remote-build note and service failure", results)
	}
	if results[1].status != "FAIL" || !strings.Contains(results[1].message, "no Dockerfile and no recognizable Nixpacks framework files") {
		t.Fatalf("service result = %#v, want unknown-framework failure", results[1])
	}
}

type fakeDoctorExecutor struct {
	command string
	output  string
	err     error
}

func (f *fakeDoctorExecutor) Execute(command string) (string, error) {
	f.command = command
	return f.output, f.err
}

func testDoctorProxyRuntimeInfo() *doctorProxyRuntimeInfo {
	return &doctorProxyRuntimeInfo{
		Image:   "caddy:2.9-alpine",
		Running: true,
		Args: []string{
			"run",
			"--config",
			"/etc/caddy/Caddyfile",
			"--adapter",
			"caddyfile",
			"--watch",
		},
		Ports: map[string]json.RawMessage{
			"80/tcp":  json.RawMessage(`[{"HostPort":"80"}]`),
			"443/tcp": json.RawMessage(`[{"HostPort":"443"}]`),
			"443/udp": json.RawMessage(`[{"HostPort":"443"}]`),
		},
		Mounts: []doctorProxyMount{
			{Source: "/etc/tako/proxy/caddy", Destination: "/etc/caddy"},
			{Source: "/etc/tako/proxy/caddy-data", Destination: "/data"},
			{Source: "/etc/tako/proxy/caddy-config", Destination: "/config"},
			{Source: "/var/log/tako/proxy", Destination: "/var/log/caddy"},
		},
		Env: []string{
			"TAKO_PROXY_EMAIL=ops@example.com",
		},
	}
}

func waitForDoctorConnectivityStarts(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := map[string]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < count {
		select {
		case name := <-started:
			seen[name] = true
		case <-deadline:
			t.Fatalf("timed out waiting for doctor connectivity fanout; saw %v", seen)
		}
	}
}

func testDoctorServers(names []string) map[string]config.ServerConfig {
	servers := make(map[string]config.ServerConfig, len(names))
	for _, name := range names {
		servers[name] = config.ServerConfig{Host: name + ".example.test", User: "root"}
	}
	return servers
}

func TestCheckNodeAgentHealthWithReportsHealthyNode(t *testing.T) {
	var results []checkResult
	checkNodeAgentHealthWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, true, func(string) (*doctorNodeHealthInfo, error) {
		return &doctorNodeHealthInfo{
			Mesh:            &mesh.Status{Interface: "tako", Up: true, Peers: 2},
			DiskUsedPercent: 41,
		}, nil
	})

	if len(results) != 3 {
		t.Fatalf("results = %#v, want three", results)
	}
	if results[0].status != "PASS" || !strings.Contains(results[0].message, "takod /healthz ok") {
		t.Fatalf("healthz result = %#v, want pass", results[0])
	}
	if results[1].status != "PASS" || !strings.Contains(results[1].message, "mesh tako up (2 peer(s))") {
		t.Fatalf("mesh result = %#v, want pass", results[1])
	}
	if results[2].status != "PASS" || !strings.Contains(results[2].message, "root filesystem 41% used") {
		t.Fatalf("disk result = %#v, want pass", results[2])
	}
}

func TestCheckNodeAgentHealthWithFlagsUnhealthyNode(t *testing.T) {
	var results []checkResult
	checkNodeAgentHealthWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, true, func(string) (*doctorNodeHealthInfo, error) {
		return &doctorNodeHealthInfo{
			HealthzErr:      "connection refused",
			Mesh:            &mesh.Status{Interface: "tako", Up: false},
			DiskUsedPercent: 97,
		}, nil
	})

	if len(results) != 3 {
		t.Fatalf("results = %#v, want three", results)
	}
	if results[0].status != "FAIL" || !strings.Contains(results[0].message, "takod /healthz unreachable") {
		t.Fatalf("healthz result = %#v, want fail", results[0])
	}
	if results[1].status != "FAIL" || !strings.Contains(results[1].message, "mesh interface is down") {
		t.Fatalf("mesh result = %#v, want fail", results[1])
	}
	if results[2].status != "FAIL" || !strings.Contains(results[2].message, "root filesystem 97% used") {
		t.Fatalf("disk result = %#v, want fail", results[2])
	}
}

func TestCheckNodeAgentHealthWithSkipsMeshWhenNotConfigured(t *testing.T) {
	var results []checkResult
	checkNodeAgentHealthWith(func(result checkResult) {
		results = append(results, result)
	}, []string{"node-a"}, false, func(string) (*doctorNodeHealthInfo, error) {
		return &doctorNodeHealthInfo{DiskUsedPercent: 88}, nil
	})

	if len(results) != 2 {
		t.Fatalf("results = %#v, want two (no mesh check)", results)
	}
	if results[1].status != "WARN" || !strings.Contains(results[1].message, "root filesystem 88% used") {
		t.Fatalf("disk result = %#v, want warn", results[1])
	}
}

func TestParseDoctorDiskUsedPercent(t *testing.T) {
	percent, err := parseDoctorDiskUsedPercent("/dev/vda1  81106868 30924356  50166128  39% /\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if percent != 39 {
		t.Fatalf("percent = %d, want 39", percent)
	}

	if _, err := parseDoctorDiskUsedPercent("garbage"); err == nil {
		t.Fatal("parse accepted malformed df output")
	}
}
