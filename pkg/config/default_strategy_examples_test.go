package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// TestExampleConfigsStillValidateWithDefaultStrategySelection is the
// "validates today" guarantee for the rolling default: every shipped example
// tako.yaml must keep loading and validating, and every service that omits
// deploy.strategy must be reported as a defaulted strategy.
func TestExampleConfigsStillValidateWithDefaultStrategySelection(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", "examples"))
	if err != nil {
		t.Fatalf("resolve examples dir: %v", err)
	}
	var configs []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "node_modules" || entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() == "tako.yaml" {
			configs = append(configs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan examples: %v", err)
	}
	if len(configs) == 0 {
		t.Fatal("no example tako.yaml files found")
	}
	sort.Strings(configs)

	home := t.TempDir()
	// Examples without sshKey fall back to ~/.ssh/id_ed25519, so point HOME at
	// a fixture instead of depending on the machine running the tests.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	sshKey := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.WriteFile(sshKey, []byte("test-key"), 0600); err != nil {
		t.Fatal(err)
	}
	relayCredentials := filepath.Join(home, "relay-credentials.json")
	if err := os.WriteFile(relayCredentials, []byte(`{"id":"validation","public_key":"validation","secret_key":"validation"}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Mirrors examples/validate.sh so the same configs resolve.
	for key, value := range map[string]string{
		"SERVER_HOST":                        "203.0.113.10",
		"TAKO_SERVER_HOST":                   "203.0.113.10",
		"SERVER1_HOST":                       "203.0.113.11",
		"SERVER2_HOST":                       "203.0.113.12",
		"SERVER_IP":                          "203.0.113.10",
		"HOSTNAME":                           "example-worker",
		"SERVER_USER":                        "root",
		"SSH_KEY":                            sshKey,
		"TAKO_SSH_KEY":                       sshKey,
		"LETSENCRYPT_EMAIL":                  "ops@example.com",
		"POSTGRES_PASSWORD":                  "example-postgres-password",
		"MYSQL_PASSWORD":                     "example-mysql-password",
		"MYSQL_ROOT_PASSWORD":                "example-mysql-root-password",
		"CLICKHOUSE_PASSWORD":                "example-clickhouse-password",
		"APP_KEY":                            "base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"APP_SECRET":                         "example-app-secret",
		"MAILER_EMAIL":                       "ops@example.com",
		"SECRET_KEY_BASE":                    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"REGISTRY_USER":                      "example-registry-user",
		"REGISTRY_TOKEN":                     "example-registry-token",
		"SENTRY_BIND":                        "9000",
		"SENTRY_MAIL_HOST":                   "sentry.example.com",
		"SENTRY_EVENT_RETENTION_DAYS":        "90",
		"SENTRY_MAX_EXTERNAL_SOURCEMAP_SIZE": "1M",
		"SENTRY_KAFKA_MAX_POLL_INTERVAL_MS":  "300000",
		"SENTRY_TASKWORKER_CONCURRENCY":      "4",
		"SENTRY_RELAY_CREDENTIALS_FILE":      relayCredentials,
	} {
		t.Setenv(key, value)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldDir) })

	for _, configPath := range configs {
		rel, _ := filepath.Rel(root, configPath)
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			if err := os.Chdir(filepath.Dir(configPath)); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Chdir(oldDir) }()
			cfg, err := LoadConfig(filepath.Base(configPath))
			if err != nil {
				t.Fatalf("LoadConfig returned error: %v", err)
			}
			for envName, env := range cfg.Environments {
				for serviceName, service := range env.Services {
					if service.IsRun() {
						if service.Deploy.Strategy != "" {
							t.Fatalf("%s/%s: run received deploy strategy %q", envName, serviceName, service.Deploy.Strategy)
						}
						continue
					}
					if service.Deploy.Strategy == "" {
						t.Fatalf("%s/%s: validated service has no deploy strategy", envName, serviceName)
					}
					if service.Deploy.StrategyDefaulted && service.Deploy.Strategy != DefaultDeployStrategy(&service) {
						t.Fatalf("%s/%s: defaulted strategy %q disagrees with DefaultDeployStrategy", envName, serviceName, service.Deploy.Strategy)
					}
				}
			}
		})
	}
}
