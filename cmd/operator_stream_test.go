package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestBuildRemoteTakodStreamCommandUsesServerSideTakoHelper(t *testing.T) {
	command := buildRemoteTakodStreamCommand("/run/tako/takod.sock", "/v1/exec", true)

	for _, want := range []string{
		"command -v tako",
		"/usr/local/bin/tako",
		"internal",
		"stream-takod",
		"--socket",
		"'/run/tako/takod.sock'",
		"--endpoint",
		"'/v1/exec'",
		"--request-stdin",
		"--stdin",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("remote stream command missing %q: %s", want, command)
		}
	}
	if strings.Contains(command, "abc123") || strings.Contains(command, "--request ") {
		t.Fatalf("remote stream command exposed request metadata: %s", command)
	}
}

func TestStreamTakodInputPrefixesRequestWithoutExposingItInCommand(t *testing.T) {
	input := streamTakodInput("secret-request", strings.NewReader("command-input"), true)
	data, err := io.ReadAll(input)
	if err != nil {
		t.Fatalf("failed to read stream input: %v", err)
	}
	if got := string(data); got != "secret-request\ncommand-input" {
		t.Fatalf("stream input = %q, want request preamble plus command stdin", got)
	}

	input = streamTakodInput("secret-request", strings.NewReader("ignored"), false)
	data, err = io.ReadAll(input)
	if err != nil {
		t.Fatalf("failed to read stream input: %v", err)
	}
	if got := string(data); got != "secret-request\n" {
		t.Fatalf("stream input = %q, want request preamble only", got)
	}
}

func TestInternalStreamRequestAndInputReadsPreamble(t *testing.T) {
	request, input, err := internalStreamRequestAndInput(strings.NewReader("request-token\npayload"), "", true)
	if err != nil {
		t.Fatalf("internalStreamRequestAndInput returned error: %v", err)
	}
	if request != "request-token" {
		t.Fatalf("request = %q, want request-token", request)
	}
	data, err := io.ReadAll(input)
	if err != nil {
		t.Fatalf("failed to read remaining input: %v", err)
	}
	if got := string(data); got != "payload" {
		t.Fatalf("remaining input = %q, want payload", got)
	}
}

func TestValidateExecTargetResponseRejectsMismatchedService(t *testing.T) {
	response := takod.ExecTargetResponse{
		Project:     "demo",
		Environment: "production",
		Service:     "worker",
		Container:   "worker-1",
		ContainerID: "container-1",
	}

	err := validateExecTargetResponse(response, "demo", "production", "web", 0)
	if err == nil {
		t.Fatal("expected service mismatch to be rejected")
	}
}

func TestEncodeTakodStreamRequestIsBase64JSON(t *testing.T) {
	encoded, err := encodeTakodStreamRequest(takod.ExecStreamRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Command:     []string{"env"},
	})
	if err != nil {
		t.Fatalf("encodeTakodStreamRequest returned error: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("encoded request is not base64: %v", err)
	}
	var decoded takod.ExecStreamRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("encoded request is not JSON: %v", err)
	}
	if decoded.Project != "demo" || decoded.Service != "web" || len(decoded.Command) != 1 {
		t.Fatalf("decoded request = %#v", decoded)
	}
}

func TestStreamTakodEndpointReturnsExitCodeTrailer(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "tako-stream-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "takod.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("failed to listen on unix socket: %v", err)
	}
	defer listener.Close()

	seenRequestHeader := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRequestHeader <- r.Header.Get("X-Tako-Request")
		w.Header().Set("Trailer", "X-Tako-Exit-Code")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello\n"))
		w.Header().Set("X-Tako-Exit-Code", "7")
	})}
	errCh := make(chan error, 1)
	go func() {
		errCh <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		select {
		case <-errCh:
		case <-time.After(2 * time.Second):
			t.Fatal("test server did not stop")
		}
	})

	var output strings.Builder
	code, err := streamTakodEndpoint(context.Background(), socket, "/v1/exec", "request-token", strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("streamTakodEndpoint returned error: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}
	if output.String() != "hello\n" {
		t.Fatalf("output = %q, want hello", output.String())
	}
	if got := <-seenRequestHeader; got != "request-token" {
		t.Fatalf("request header = %q, want request-token", got)
	}
}

func TestCommandWantsTTYDefaultsOnlyForShell(t *testing.T) {
	if commandWantsTTY([]string{"env"}, false) {
		t.Fatal("explicit command should not allocate TTY by default")
	}
	if !commandWantsTTY([]string{"env"}, true) {
		t.Fatal("explicit TTY flag should allocate TTY")
	}
}

func TestCommandWantsStdinForPipedInput(t *testing.T) {
	file, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("failed to open devnull: %v", err)
	}
	defer file.Close()
	oldStdin := os.Stdin
	os.Stdin = file
	t.Cleanup(func() { os.Stdin = oldStdin })

	if commandWantsStdin([]string{"env"}, false, false) {
		t.Fatal("explicit command should not forward stdin by default")
	}
	if !commandWantsStdin([]string{"env"}, false, true) {
		t.Fatal("explicit stdin flag should forward stdin")
	}
	if !commandWantsStdin(nil, false, false) {
		t.Fatal("default shell should forward non-terminal stdin")
	}
}

func TestBuildOperatorMountSpecsUsesTopLevelVolumeName(t *testing.T) {
	cfg := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Volumes: map[string]config.VolumeConfig{
			"cache": {Name: "shared-cache"},
		},
	}

	mounts, err := buildOperatorMountSpecs(cfg, "production", "job", &config.ServiceConfig{
		Volumes: []string{"/data", "cache:/cache", "cache:/cache-ro:ro", "/srv/app/config:/config:ro"},
	})
	if err != nil {
		t.Fatalf("buildOperatorMountSpecs returned error: %v", err)
	}

	want := []string{
		"type=volume,source=" + runtimeid.VolumeName("demo", "production", "/data") + ",target=/data",
		"type=volume,source=shared-cache,target=/cache",
		"type=volume,source=shared-cache,target=/cache-ro,readonly",
		"type=bind,source=/srv/app/config,target=/config,readonly",
	}
	if !slices.Equal(mounts, want) {
		t.Fatalf("mounts = %#v, want %#v", mounts, want)
	}
}

func TestBuildOperatorMountSpecsRejectsRelativeBindMount(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}

	_, err := buildOperatorMountSpecs(cfg, "production", "job", &config.ServiceConfig{
		Volumes: []string{"./data:/data"},
	})
	if err == nil {
		t.Fatal("buildOperatorMountSpecs should reject relative bind mounts")
	}
	if !strings.Contains(err.Error(), "relative bind mount") {
		t.Fatalf("error = %q, want relative bind mount guidance", err)
	}
}

func TestCommandRegistryAuthMatchesConfiguredRegistry(t *testing.T) {
	cfg := &config.Config{
		Registry: &config.RegistryConfig{
			URL:      "ghcr.io",
			Username: "deploy",
			Password: "secret-token",
		},
	}

	auth := commandRegistryAuth(cfg, "ghcr.io/redentor/app:latest")
	if auth == nil {
		t.Fatal("expected registry auth")
	}
	if auth.Server != "ghcr.io" || auth.Username != "deploy" || auth.Password != "secret-token" {
		t.Fatalf("unexpected registry auth: %#v", auth)
	}
}
