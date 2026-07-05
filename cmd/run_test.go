package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/spf13/cobra"
)

func TestSynthesizeRunConfigShape(t *testing.T) {
	opts := runOptions{
		Name:        "web",
		Port:        80,
		Server:      "prod-1.example.com",
		ServerName:  "edge-1",
		Environment: "production",
		User:        "deploy",
		SSHKey:      "~/.ssh/id_rsa",
		Password:    "${PW}",
		SSHPort:     2222,
		Domain:      "example.com",
		Replicas:    2,
		Env:         []string{"FOO=bar", "SECRET=s3cr3t"},
	}
	cfg, service, envVars, err := synthesizeRunConfig("nginx:1.27", &opts)
	if err != nil {
		t.Fatalf("synthesizeRunConfig returned error: %v", err)
	}

	wantVersion, err := deployplan.ImageBuildTag("", "nginx:1.27")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Project.Name != "web" || cfg.Project.Version != wantVersion {
		t.Fatalf("project = %#v, want web/%s", cfg.Project, wantVersion)
	}
	server, ok := cfg.Servers["edge-1"]
	if !ok {
		t.Fatalf("server edge-1 missing: %#v", cfg.Servers)
	}
	if server.Host != "prod-1.example.com" || server.User != "deploy" || server.Port != 2222 || server.Password != "${PW}" {
		t.Fatalf("server = %#v", server)
	}
	env, ok := cfg.Environments["production"]
	if !ok {
		t.Fatalf("environment production missing: %#v", cfg.Environments)
	}
	if len(env.Servers) != 1 || env.Servers[0] != "edge-1" {
		t.Fatalf("environment servers = %#v", env.Servers)
	}
	if service.Image != "nginx:1.27" || service.Build != "" || service.Port != 80 || service.Replicas != 2 {
		t.Fatalf("service = %#v", service)
	}
	if service.Proxy == nil || service.Proxy.Domain != "example.com" || service.Proxy.Visibility != config.ProxyVisibilityPublic {
		t.Fatalf("proxy = %#v", service.Proxy)
	}
	if envVars["FOO"] != "bar" || envVars["SECRET"] != "s3cr3t" {
		t.Fatalf("env vars = %#v", envVars)
	}
	redacted := redactedEnvKeys(service.Env)
	if redacted["FOO"] != "<redacted>" || redacted["SECRET"] != "<redacted>" {
		t.Fatalf("redacted env = %#v", redacted)
	}
}

func TestSynthesizeRunConfigDefaults(t *testing.T) {
	opts := runOptions{Name: "web", Port: 8080, Server: "prod-1", User: "deploy", Password: "${PW}"}
	cfg, service, _, err := synthesizeRunConfig("nginx", &opts)
	if err != nil {
		t.Fatalf("synthesizeRunConfig returned error: %v", err)
	}
	if opts.Environment != "production" || opts.Replicas != 1 || opts.SSHPort != 22 {
		t.Fatalf("unexpected normalized opts: %#v", opts)
	}
	if cfg.Environments["production"].Servers[0] != "prod-1" {
		t.Fatalf("server name = %#v", cfg.Environments["production"].Servers)
	}
	if service.Replicas != 1 {
		t.Fatalf("service replicas = %d, want validation default 1", service.Replicas)
	}
	if service.Proxy != nil {
		t.Fatalf("proxy = %#v, want nil without --domain", service.Proxy)
	}
}

func TestParseRunEnvVars(t *testing.T) {
	got, err := parseRunEnvVars([]string{"KEY=value=with=equals", "EMPTY="})
	if err != nil {
		t.Fatalf("parseRunEnvVars returned error: %v", err)
	}
	if got["KEY"] != "value=with=equals" || got["EMPTY"] != "" {
		t.Fatalf("env = %#v", got)
	}

	invalid := [][]string{{"NOVALUE"}, {"=value"}, {"BAD KEY=value"}}
	for _, values := range invalid {
		if _, err := parseRunEnvVars(values); err == nil {
			t.Fatalf("parseRunEnvVars(%#v) returned nil error", values)
		}
	}
}

func TestSynthesizeRunConfigValidationErrors(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		opts    runOptions
		wantErr string
	}{
		{name: "empty image", image: " ", opts: validRunOptions(), wantErr: "IMAGE is required"},
		{name: "image starts dash", image: "-bad", opts: validRunOptions(), wantErr: "IMAGE must not start"},
		{name: "image whitespace", image: "bad image", opts: validRunOptions(), wantErr: "IMAGE contains unsupported"},
		{name: "missing name", image: "nginx", opts: runOptions{Port: 80, Server: "prod", User: "deploy", Replicas: 1}, wantErr: "--name is required"},
		{name: "invalid name", image: "nginx", opts: runOptions{Name: "Web", Port: 80, Server: "prod", User: "deploy", Replicas: 1}, wantErr: "project name"},
		{name: "invalid port", image: "nginx", opts: runOptions{Name: "web", Port: 0, Server: "prod", User: "deploy", Replicas: 1}, wantErr: "--port must be greater than 0"},
		{name: "missing server", image: "nginx", opts: runOptions{Name: "web", Port: 80, User: "deploy", Replicas: 1}, wantErr: "--server is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			_, _, _, err := synthesizeRunConfig(tt.image, &opts)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRunCommandUsesFakeRunnerAfterSynthesis(t *testing.T) {
	oldRunner := runRunner
	defer func() { runRunner = oldRunner }()
	called := false
	runRunner = func(cmd *cobra.Command, imageRef string, opts runOptions, cfg *config.Config, service config.ServiceConfig, envVars map[string]string) error {
		called = true
		if imageRef != "nginx:1.27" || opts.Name != "web" || service.Image != "nginx:1.27" || envVars["FOO"] != "bar" {
			t.Fatalf("unexpected runner args: image=%q opts=%#v service=%#v env=%#v", imageRef, opts, service, envVars)
		}
		if cfg.Project.Name != "web" || cfg.Environments["production"].Servers[0] == "" {
			t.Fatalf("unexpected config: %#v", cfg)
		}
		return nil
	}

	cmd := newRunCommand()
	cmd.SetArgs([]string{"nginx:1.27", "--name", "web", "--port", "80", "--server", "prod-1", "--user", "deploy", "--password", "${PW}", "--env", "FOO=bar"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("command returned error: %v", err)
	}
	if !called {
		t.Fatal("fake runner was not called")
	}
}

func validRunOptions() runOptions {
	return runOptions{Name: "web", Port: 80, Server: "prod", User: "deploy", Password: "${PW}", Replicas: 1}
}
