package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestSetupTargetServersUsesOnlyEnvironmentNodesByDefault(t *testing.T) {
	cfg := resolverConfig()

	names, servers, err := setupTargetServers(cfg, "production", "")
	if err != nil {
		t.Fatalf("setupTargetServers returned error: %v", err)
	}

	if !slices.Equal(names, []string{"node-a", "node-b"}) {
		t.Fatalf("server names = %#v, want production nodes", names)
	}
	if _, ok := servers["node-c"]; ok {
		t.Fatal("node-c should not be targeted because it is outside production")
	}
}

func TestSetupTargetServersRequiresRequestedServerInEnvironment(t *testing.T) {
	cfg := resolverConfig()

	if _, _, err := setupTargetServers(cfg, "production", "node-c"); err == nil {
		t.Fatal("setupTargetServers should reject a server outside the environment")
	}
}

func TestSetupMeshListenPort(t *testing.T) {
	if got := setupMeshListenPort(&config.Config{}); got != 51820 {
		t.Fatalf("default mesh listen port = %d, want 51820", got)
	}

	cfg := &config.Config{Mesh: &config.MeshConfig{ListenPort: 42420}}
	if got := setupMeshListenPort(cfg); got != 42420 {
		t.Fatalf("configured mesh listen port = %d, want 42420", got)
	}
}

func TestSetupRegistersTakodBinaryFlag(t *testing.T) {
	flag := setupCmd.Flags().Lookup("takod-binary")
	if flag == nil {
		t.Fatal("setup command should expose --takod-binary")
	}
	if !strings.Contains(flag.Usage, "development/testing") {
		t.Fatalf("takod-binary flag usage = %q, want development/testing context", flag.Usage)
	}
}

func TestSetupRegistersDedicatedEdgeFlag(t *testing.T) {
	flag := setupCmd.Flags().Lookup("dedicated-edge")
	if flag == nil {
		t.Fatal("setup command should expose --dedicated-edge")
	}
	if !strings.Contains(flag.Usage, "80/443") {
		t.Fatalf("dedicated-edge flag usage = %q, want direct edge context", flag.Usage)
	}
}

func TestSetupFeaturesSwitchForDedicatedEdge(t *testing.T) {
	regular := setupFeatures(false)
	if !slices.Contains(regular, "tako-proxy") || slices.Contains(regular, "dedicated-edge") {
		t.Fatalf("regular setup features = %#v", regular)
	}
	edge := setupFeatures(true)
	if !slices.Contains(edge, "dedicated-edge") || slices.Contains(edge, "tako-proxy") {
		t.Fatalf("dedicated edge features = %#v", edge)
	}
}

func TestSetupVersionWriteErrorFailsSuccessfulProvisioning(t *testing.T) {
	err := setupVersionWriteError("node-a", errors.New("permission denied"))
	if err == nil {
		t.Fatal("setupVersionWriteError returned nil")
	}
	for _, want := range []string{"node-a", "setup completed", "failed to write setup version metadata", "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err, want)
		}
	}
}

func TestRefreshCurrentSetupRegrantsDeployUserAccessBeforeTakodRestart(t *testing.T) {
	oldTakodBinary := setupTakodBinary
	setupTakodBinary = ""
	t.Cleanup(func() {
		setupTakodBinary = oldTakodBinary
	})

	cfg := &config.Config{
		Runtime: &config.RuntimeConfig{
			Agent: &config.AgentConfig{
				Socket:  "/run/custom/takod.sock",
				DataDir: "/var/lib/custom-tako",
			},
		},
	}
	prov := &recordingSetupRefresher{}

	if err := refreshCurrentSetup(prov, cfg, "node-a", "deploy", 42420); err != nil {
		t.Fatalf("refreshCurrentSetup returned error: %v", err)
	}

	want := []string{
		"firewall:42420",
		"buildx",
		"deploy-user:deploy",
		"install-release:" + Version,
		"service:/run/custom/takod.sock:/var/lib/custom-tako:node-a",
	}
	if !slices.Equal(prov.calls, want) {
		t.Fatalf("calls = %#v, want %#v", prov.calls, want)
	}
}

func TestRefreshCurrentSetupWrapsBuildxError(t *testing.T) {
	prov := &recordingSetupRefresher{buildxErr: errors.New("apt failed")}

	err := refreshCurrentSetup(prov, &config.Config{}, "node-a", "deploy", 51820)
	if err == nil {
		t.Fatal("refreshCurrentSetup returned nil error")
	}
	if !strings.Contains(err.Error(), "refresh docker buildx") || !strings.Contains(err.Error(), "apt failed") {
		t.Fatalf("error = %q, want buildx context", err)
	}
}

func TestRefreshCurrentSetupWrapsDeployUserAccessError(t *testing.T) {
	prov := &recordingSetupRefresher{deployUserErr: errors.New("usermod failed")}

	err := refreshCurrentSetup(prov, &config.Config{}, "node-a", "deploy", 51820)
	if err == nil {
		t.Fatal("refreshCurrentSetup returned nil error")
	}
	if !strings.Contains(err.Error(), "refresh deploy user access") || !strings.Contains(err.Error(), "usermod failed") {
		t.Fatalf("error = %q, want deploy access context", err)
	}
}

func TestDisableTakodProxyForSetupDecodesSuccess(t *testing.T) {
	executor := &recordingTakodExecutor{
		outputs: []string{`{"container":"tako-proxy","removed":true}`},
	}
	err := disableTakodProxyForSetup(executor, &config.Config{}, "edge")
	if err != nil {
		t.Fatalf("disableTakodProxyForSetup returned error: %v", err)
	}
	if !slices.Equal(executor.methods, []string{"DELETE"}) || !slices.Equal(executor.endpoints, []string{"/v1/proxy"}) {
		t.Fatalf("requests = %#v %#v, want DELETE /v1/proxy", executor.methods, executor.endpoints)
	}
}

func TestDisableTakodProxyForSetupRetriesUnavailableSocket(t *testing.T) {
	executor := &recordingTakodExecutor{
		outputs: []string{
			"takod socket or curl is unavailable\n__TAKO_HTTP_STATUS__:000",
			`{"container":"tako-proxy","removed":false}`,
		},
		errs: []error{
			errors.New("exit status 42"),
			nil,
		},
	}
	err := disableTakodProxyForSetup(executor, &config.Config{}, "edge")
	if err != nil {
		t.Fatalf("disableTakodProxyForSetup returned error: %v", err)
	}
	if len(executor.methods) != 2 {
		t.Fatalf("request count = %d, want retry", len(executor.methods))
	}
}

func TestDisableTakodProxyForSetupDoesNotRetryActiveRoutes(t *testing.T) {
	executor := &recordingTakodExecutor{
		outputs: []string{
			"cannot disable tako-proxy while proxy route files exist: demo.yml\n__TAKO_HTTP_STATUS__:400",
			`{"container":"tako-proxy","removed":true}`,
		},
	}
	err := disableTakodProxyForSetup(executor, &config.Config{}, "edge")
	if err == nil {
		t.Fatal("expected active proxy routes to fail")
	}
	if len(executor.methods) != 1 {
		t.Fatalf("request count = %d, want no retry for active routes", len(executor.methods))
	}
}

func TestEnableTakodProxyForSetupReconcilesSharedProxy(t *testing.T) {
	executor := &recordingTakodExecutor{
		outputs: []string{`{"container":"tako-proxy","image":"traefik:v3.6.1"}`},
	}
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo-app"}}

	err := enableTakodProxyForSetup(executor, cfg, "production", "node-a")
	if err != nil {
		t.Fatalf("enableTakodProxyForSetup returned error: %v", err)
	}
	if !slices.Equal(executor.methods, []string{"POST"}) || !slices.Equal(executor.endpoints, []string{"/v1/proxy"}) {
		t.Fatalf("requests = %#v %#v, want POST /v1/proxy", executor.methods, executor.endpoints)
	}
	if !strings.Contains(executor.requests[0], "tako_demo_app_production") {
		t.Fatalf("proxy reconcile request should include setup network name: %s", executor.requests[0])
	}
}

type recordingSetupRefresher struct {
	calls         []string
	firewallErr   error
	buildxErr     error
	deployUserErr error
	releaseErr    error
	fileErr       error
	serviceErr    error
}

func (r *recordingSetupRefresher) ConfigureFirewall(meshListenPort int) error {
	r.calls = append(r.calls, fmt.Sprintf("firewall:%d", meshListenPort))
	return r.firewallErr
}

func (r *recordingSetupRefresher) EnsureDockerBuildx() error {
	r.calls = append(r.calls, "buildx")
	return r.buildxErr
}

func (r *recordingSetupRefresher) SetupDeployUser(username string) error {
	r.calls = append(r.calls, "deploy-user:"+username)
	return r.deployUserErr
}

func (r *recordingSetupRefresher) InstallTakodBinary(version string) error {
	r.calls = append(r.calls, "install-release:"+version)
	return r.releaseErr
}

func (r *recordingSetupRefresher) InstallTakodBinaryFromFile(path string) error {
	r.calls = append(r.calls, "install-file:"+path)
	return r.fileErr
}

func (r *recordingSetupRefresher) InstallTakodService(socket string, dataDir string, nodeName string) error {
	r.calls = append(r.calls, "service:"+socket+":"+dataDir+":"+nodeName)
	return r.serviceErr
}

type recordingTakodExecutor struct {
	outputs   []string
	errs      []error
	methods   []string
	endpoints []string
	requests  []string
	calls     int
}

func (r *recordingTakodExecutor) ExecuteWithContext(_ context.Context, cmd string) (string, error) {
	r.record(cmd)
	return r.response()
}

func (r *recordingTakodExecutor) ExecuteWithInput(_ context.Context, cmd string, input io.Reader) (string, error) {
	body, _ := io.ReadAll(input)
	r.record(cmd + "\n" + string(body))
	return r.response()
}

func (r *recordingTakodExecutor) response() (string, error) {
	index := r.calls - 1
	output := ""
	if index < len(r.outputs) {
		output = r.outputs[index]
	}
	var err error
	if index < len(r.errs) {
		err = r.errs[index]
	}
	return output, err
}

func (r *recordingTakodExecutor) record(cmd string) {
	r.calls++
	if strings.Contains(cmd, "-X 'DELETE'") {
		r.methods = append(r.methods, "DELETE")
	} else if strings.Contains(cmd, "-X 'POST'") {
		r.methods = append(r.methods, "POST")
	} else {
		r.methods = append(r.methods, "unknown")
	}
	if strings.Contains(cmd, "http://takod/v1/proxy") {
		r.endpoints = append(r.endpoints, "/v1/proxy")
	} else {
		r.endpoints = append(r.endpoints, "")
	}
	r.requests = append(r.requests, cmd)
}
