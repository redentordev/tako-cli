package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfigAcceptsOperatorFileAndDirectorySources(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "geoip.mmdb")
	if err := os.WriteFile(file, []byte{0x00, 0xff, 0x10}, 0644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sentry")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("key: value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	service := ServiceConfig{Image: "busybox:1.36", Files: []ServiceFileConfig{
		{Source: file, Target: "/usr/share/GeoIP/geoip.mmdb"},
		{Source: dir, Target: "/etc/sentry", Secret: true},
	}}
	cfg := minimalValidConfigWithService(service)
	if err := ValidateConfig(cfg); err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
}

func TestValidateConfigRejectsUnsafeOperatorFiles(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "config")
	if err := os.WriteFile(file, []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		files []ServiceFileConfig
		want  string
	}{
		{name: "relative target", files: []ServiceFileConfig{{Source: file, Target: "etc/config"}}, want: "absolute container path"},
		{name: "duplicate target", files: []ServiceFileConfig{{Source: file, Target: "/etc/config"}, {Source: file, Target: "/etc/config"}}, want: "duplicated"},
		{name: "missing source", files: []ServiceFileConfig{{Source: filepath.Join(root, "missing"), Target: "/etc/config"}}, want: "not accessible"},
		{name: "invalid owner", files: []ServiceFileConfig{{Source: file, Target: "/etc/config", Owner: "relay"}}, want: "owner uid must be numeric"},
	}
	cfg := minimalValidConfigWithService(ServiceConfig{Image: "busybox", Volumes: []string{"data:/etc/config"}, Files: []ServiceFileConfig{{Source: file, Target: "/etc/config"}}})
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "conflicts with a volume") {
		t.Fatalf("mount collision error = %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalValidConfigWithService(ServiceConfig{Image: "busybox", Files: tt.files})
			err := ValidateConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	symlink := filepath.Join(root, "link")
	if err := os.Symlink(file, symlink); err == nil {
		cfg := minimalValidConfigWithService(ServiceConfig{Image: "busybox", Files: []ServiceFileConfig{{Source: symlink, Target: "/etc/config"}}})
		if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink error = %v", err)
		}
	}
}

func TestLoadConfigResolvesOperatorFileSourceRelativeToConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "nginx.conf"), []byte("events {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "id_rsa")
	if err := os.WriteFile(key, []byte("key"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "tako.yaml")
	yaml := `project:
  name: demo
  version: "1"
servers:
  node1:
    host: 127.0.0.1
    user: root
    sshKey: ./id_rsa
environments:
  production:
    servers: [node1]
    services:
      web:
        image: nginx:1.27
        files:
          - source: ./nginx.conf
            target: /etc/nginx/nginx.conf
`
	if err := os.WriteFile(configPath, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Environments["production"].Services["web"].Files[0].Source
	if got != filepath.Join(root, "nginx.conf") {
		t.Fatalf("resolved source = %q", got)
	}
}
