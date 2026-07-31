package takod

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaddyAllowPathPrefixesConformance(t *testing.T) {
	pinned := os.Getenv("TAKO_TEST_PINNED_CADDY") == "1"
	caddy := ""
	if !pinned {
		var err error
		caddy, err = exec.LookPath("caddy")
		if err != nil {
			t.Skip("caddy is not installed")
		}
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer backend.Close()

	publicHTTPAddress := freeCaddyTestAddress(t)
	publicServeAddress := freeCaddyTestAddress(t)
	redirectAddress := freeCaddyTestAddress(t)
	internalAddress := freeCaddyTestAddress(t)
	dynamicServeAddress := freeCaddyTestAddress(t)
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{{
		Version: 1, Project: "demo", Environment: "production",
		Routes: []ProxyRoute{
			{
				Service: "web", Domains: []string{"example.test"}, RedirectFrom: []string{"old.example.test"},
				Upstreams: []string{backend.URL}, DenyPathPrefixes: []string{"/api/internal"}, AllowPathPrefixes: []string{"/api", "/health"},
			},
			{
				Service: "internal", Domains: []string{internalAddress}, Visibility: proxyRouteVisibilityInternal,
				Upstreams: []string{backend.URL}, DenyPathPrefixes: []string{"/api/internal"}, AllowPathPrefixes: []string{"/api", "/health"},
			},
			{
				Service: "dynamic", DynamicDomain: &ProxyDynamicDomain{AskURL: backend.URL},
				Upstreams: []string{backend.URL}, DenyPathPrefixes: []string{"/api/internal"}, AllowPathPrefixes: []string{"/api", "/health"},
			},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	caddyfile = strings.Replace(caddyfile, "{\n", "{\n\tadmin off\n", 1)
	caddyfile = strings.Replace(caddyfile, ":80 {", "http://:"+caddyTestPort(t, publicHTTPAddress)+" {", 1)
	caddyfile = strings.Replace(caddyfile, "\nexample.test {", "\nhttp://example.test:"+caddyTestPort(t, publicServeAddress)+" {", 1)
	caddyfile = strings.Replace(caddyfile, "\nold.example.test {", "\nhttp://old.example.test:"+caddyTestPort(t, redirectAddress)+" {", 1)
	caddyfile = strings.Replace(caddyfile, "\n:443 {", "\nhttp://:"+caddyTestPort(t, dynamicServeAddress)+" {", 1)
	logPath := filepath.Join(t.TempDir(), "access.log")
	if pinned {
		logPath = "/tmp/tako-access.log"
		caddyfile = strings.ReplaceAll(caddyfile, "reverse_proxy "+backend.URL, "respond 204")
		caddyfile = strings.Replace(caddyfile, "http://"+internalAddress+" {", "http://:"+caddyTestPort(t, internalAddress)+" {", 1)
	}
	caddyfile = strings.ReplaceAll(caddyfile, "/var/log/caddy/access.log", logPath)
	configPath := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(configPath, []byte(caddyfile), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var output bytes.Buffer
	var cmd *exec.Cmd
	containerName := "tako-caddy-conformance-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if pinned {
		docker, err := exec.LookPath("docker")
		if err != nil {
			t.Skip("docker is not installed")
		}
		args := []string{"run", "--rm", "-i", "--name", containerName, "-e", "TAKO_PROXY_EMAIL=test@example.com"}
		for _, address := range []string{publicHTTPAddress, publicServeAddress, redirectAddress, internalAddress, dynamicServeAddress} {
			port := caddyTestPort(t, address)
			args = append(args, "-p", "127.0.0.1:"+port+":"+port)
		}
		args = append(args, defaultProxyImage, "caddy", "run", "--config", "-", "--adapter", "caddyfile")
		cmd = exec.CommandContext(ctx, docker, args...)
		cmd.Stdin = strings.NewReader(caddyfile)
		defer func() { _ = exec.Command(docker, "rm", "-f", containerName).Run() }()
	} else {
		cmd = exec.CommandContext(ctx, caddy, "run", "--config", configPath, "--adapter", "caddyfile")
		cmd.Env = append(os.Environ(), "TAKO_PROXY_EMAIL=test@example.com")
	}
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	baseURL := "http://" + internalAddress
	deadline := time.Now().Add(30 * time.Second)
	for {
		response, requestErr := http.Get(baseURL + "/health")
		if requestErr == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusNoContent {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("caddy did not become ready: %v\n%s", requestErr, output.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	for _, surface := range []struct {
		name        string
		address     string
		host        string
		allowedCode int
	}{
		{name: "public-http", address: publicHTTPAddress, host: "example.test", allowedCode: http.StatusPermanentRedirect},
		{name: "public-serve", address: publicServeAddress, host: "example.test", allowedCode: http.StatusNoContent},
		{name: "redirect-alias", address: redirectAddress, host: "old.example.test", allowedCode: http.StatusPermanentRedirect},
		{name: "internal-http", address: internalAddress, host: internalAddress, allowedCode: http.StatusNoContent},
		{name: "dynamic-http", address: publicHTTPAddress, host: "tenant.example.test", allowedCode: http.StatusPermanentRedirect},
		{name: "dynamic-serve", address: dynamicServeAddress, host: "tenant.example.test", allowedCode: http.StatusNoContent},
	} {
		t.Run(surface.name, func(t *testing.T) {
			for _, test := range []struct {
				path string
				want int
			}{
				{path: "/api", want: surface.allowedCode},
				{path: "/api/users", want: surface.allowedCode},
				{path: "/health", want: surface.allowedCode},
				{path: "/API", want: http.StatusNotFound},
				{path: "/apiized", want: http.StatusNotFound},
				{path: "/private", want: http.StatusNotFound},
				{path: "/api/internal", want: http.StatusNotFound},
				{path: "/api/internal/users", want: http.StatusNotFound},
				{path: "/api/internal%2Fusers", want: http.StatusNotFound},
				{path: "/api//internal/users", want: http.StatusNotFound},
			} {
				t.Run(fmt.Sprintf("%s_%d", test.path, test.want), func(t *testing.T) {
					req, err := http.NewRequest(http.MethodGet, "http://"+surface.address+test.path, nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Host = surface.host
					response, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					if response.StatusCode != test.want {
						t.Fatalf("GET %s status = %d, want %d, headers=%v\n%s\n%s", test.path, response.StatusCode, test.want, response.Header, caddyfile, output.String())
					}
				})
			}
		})
	}
}

func freeCaddyTestAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

func caddyTestPort(t *testing.T, address string) string {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestPinnedCaddyAllowPathConfigConformance(t *testing.T) {
	if os.Getenv("TAKO_TEST_PINNED_CADDY") != "1" {
		t.Skip("set TAKO_TEST_PINNED_CADDY=1 to validate with the production Caddy image")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker is not installed")
	}
	caddyfile, err := renderCaddyfile([]ProxyRouteManifest{{
		Version: 1, Project: "demo", Environment: "production",
		Routes: []ProxyRoute{{
			Service: "web", Domains: []string{"example.test"}, RedirectFrom: []string{"old.example.test"},
			Upstreams: []string{"http://web:3000"}, DenyPathPrefixes: []string{"/api/internal"}, AllowPathPrefixes: []string{"/api", "/health"},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(docker, "run", "--rm", "-i", "-e", "TAKO_PROXY_EMAIL=test@example.com", defaultProxyImage, "caddy", "adapt", "--adapter", "caddyfile", "--config", "-")
	cmd.Stdin = strings.NewReader(caddyfile)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pinned Caddy rejected allow-path config: %v\n%s", err, output)
	}
}
