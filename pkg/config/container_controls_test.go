package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestServiceConfigStructuredBuildAndRuntimeControlsRoundTrip(t *testing.T) {
	input := `build:
  context: ./sentry
  args:
    SENTRY_IMAGE: getsentry/sentry:26.6.0
  target: runtime
envFiles: [.env.base, .env.production]
user: "1000:1000"
workingDir: /work
stopGracePeriod: 60s
init: true
extraHosts: [host.docker.internal:host-gateway]
ulimits:
  nofile:
    soft: 262144
    hard: 262144
  nproc: 4096
shmSize: 256m
`
	var service ServiceConfig
	if err := yaml.Unmarshal([]byte(input), &service); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if service.Build != "./sentry" || service.BuildArgs["SENTRY_IMAGE"] != "getsentry/sentry:26.6.0" || service.BuildTarget != "runtime" {
		t.Fatalf("structured build = %#v", service)
	}
	if service.Ulimits["nproc"] != (UlimitConfig{Soft: 4096, Hard: 4096}) {
		t.Fatalf("scalar ulimit = %#v", service.Ulimits["nproc"])
	}
	encoded, err := yaml.Marshal(service)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{"context: ./sentry", "SENTRY_IMAGE: getsentry/sentry:26.6.0", "target: runtime", "envFiles:", "shmSize: 256m"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("marshaled service missing %q:\n%s", want, encoded)
		}
	}
}

func TestServiceConfigStructuredBuildJSONRoundTripIsStrict(t *testing.T) {
	input := `{"build":{"context":".","args":{"BASE":"alpine"},"target":"runtime"},"image":""}`
	var service ServiceConfig
	if err := json.Unmarshal([]byte(input), &service); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if service.Build != "." || service.BuildArgs["BASE"] != "alpine" || service.BuildTarget != "runtime" {
		t.Fatalf("structured JSON build = %#v", service)
	}
	data, err := json.Marshal(service)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	for _, want := range []string{`"context":"."`, `"args":{"BASE":"alpine"}`, `"target":"runtime"`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("JSON missing %s: %s", want, data)
		}
	}
	for _, bad := range []string{
		`{"build":{},"image":"nginx:latest"}`,
		`{"build":{"context":""},"image":"nginx:latest"}`,
		`{"build":{"context":".","unknown":true}}`,
		`{"build":".","unknown":true}`,
	} {
		if err := json.Unmarshal([]byte(bad), &service); err == nil {
			t.Fatalf("unknown JSON field accepted: %s", bad)
		}
	}
}

func TestServiceConfigStructuredBuildKeepsStrictUnknownFieldChecks(t *testing.T) {
	for _, input := range []string{
		"build: {}\nimage: nginx:latest\n",
		"build:\n  context: \"\"\nimage: nginx:latest\n",
		"build:\n  context: .\n  mystery: value\n",
		"build: .\nmystery: value\n",
		"build: .\nulimits:\n  nofile:\n    soft: 1\n    typo: 2\n",
	} {
		var service ServiceConfig
		if err := yaml.Unmarshal([]byte(input), &service); err == nil {
			t.Fatalf("unknown field accepted:\n%s", input)
		}
	}
}

func TestUlimitConfigJSONScalarAndObjectParity(t *testing.T) {
	var scalar UlimitConfig
	if err := json.Unmarshal([]byte(`262144`), &scalar); err != nil {
		t.Fatalf("scalar JSON ulimit: %v", err)
	}
	if scalar.Soft != 262144 || scalar.Hard != 262144 {
		t.Fatalf("scalar JSON ulimit = %#v", scalar)
	}
	var object UlimitConfig
	if err := json.Unmarshal([]byte(`{"soft":1024,"hard":2048}`), &object); err != nil {
		t.Fatalf("object JSON ulimit: %v", err)
	}
	if object.Soft != 1024 || object.Hard != 2048 {
		t.Fatalf("object JSON ulimit = %#v", object)
	}
	if err := json.Unmarshal([]byte(`{"soft":1,"typo":2}`), &object); err == nil {
		t.Fatal("unknown JSON ulimit field accepted")
	}
}

func TestValidateServiceContainerControlsAndOrderedEnvFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"base.env", "override.env"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("VALUE="+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	service := ServiceConfig{
		Build:           root,
		BuildArgs:       map[string]string{"BASE_IMAGE": "alpine:3.20"},
		BuildTarget:     "runtime",
		EnvFiles:        []string{filepath.Join(root, "base.env"), filepath.Join(root, "override.env")},
		User:            "1000:1000",
		WorkingDir:      "/work",
		StopGracePeriod: "60s",
		Init:            true,
		ExtraHosts:      []string{"host.docker.internal:host-gateway", "database:10.0.0.2"},
		Ulimits:         map[string]UlimitConfig{"nofile": {Soft: 262144, Hard: 262144}},
		ShmSize:         "256M",
	}
	cfg := minimalValidConfigWithService(service)
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	got := cfg.Environments["production"].Services["web"]
	if got.ShmSize != "256m" {
		t.Fatalf("normalized shmSize = %q", got.ShmSize)
	}
}

func TestNormalizeContainerWorkingDirUsesPOSIXSemantics(t *testing.T) {
	got, err := normalizeContainerWorkingDir("/var/lib/../work")
	if err != nil || got != "/var/work" {
		t.Fatalf("POSIX working dir = %q, %v", got, err)
	}
	for _, invalid := range []string{"work", `C:\\work`} {
		if _, err := normalizeContainerWorkingDir(invalid); err == nil {
			t.Fatalf("non-POSIX working dir %q accepted", invalid)
		}
	}
}

func minimalValidConfigWithService(service ServiceConfig) *Config {
	return &Config{
		Project: ProjectConfig{Name: "demo", Version: "1"},
		Servers: map[string]ServerConfig{"node": {Host: "127.0.0.1", User: "root", Password: "test"}},
		Environments: map[string]EnvironmentConfig{
			"production": {Servers: []string{"node"}, Services: map[string]ServiceConfig{"web": service}},
		},
	}
}
