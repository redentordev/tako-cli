package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
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
  - Server connectivity (skip with --skip-remote)
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

	record := func(r checkResult) {
		switch r.status {
		case "PASS":
			passed++
		case "WARN":
			warned++
		case "FAIL":
			failed++
		}
		fmt.Printf("  [%s] %s", r.status, r.message)
		if r.fix != "" {
			fmt.Printf(" (Fix: %s)", r.fix)
		}
		fmt.Println()
	}

	// === Configuration ===
	fmt.Println("=== Configuration ===")
	cfg, cfgErr := checkConfig(record)

	if cfgErr != nil {
		fmt.Printf("\nSummary: %d passed, %d warning(s), %d failed\n", passed, warned, failed)
		return nil
	}

	envName := getEnvironmentName(cfg)

	// === Environment ===
	fmt.Println("\n=== Environment ===")
	checkEnvFile(record)

	// === SSH Keys ===
	fmt.Println("\n=== SSH Keys ===")
	checkSSHKeys(record, cfg, envName)

	// === Secrets ===
	fmt.Println("\n=== Secrets ===")
	checkSecrets(record, cfg, envName)

	// === Local State ===
	fmt.Println("\n=== Local State ===")
	checkLocalState(record)

	// === Server Connectivity ===
	if !doctorSkipRemote {
		fmt.Println("\n=== Server Connectivity ===")
		clients := checkServerConnectivity(record, cfg, envName)

		// === Running Services ===
		fmt.Println("\n=== Running Services ===")
		checkRunningServices(record, cfg, envName, clients)

		// Clean up clients
		for _, c := range clients {
			c.Close()
		}
	} else {
		fmt.Println("\n=== Server Connectivity ===")
		fmt.Println("  [SKIP] Skipped (--skip-remote)")
		fmt.Println("\n=== Running Services ===")
		fmt.Println("  [SKIP] Skipped (--skip-remote)")
	}

	// Summary
	fmt.Printf("\nSummary: %d passed, %d warning(s), %d failed\n", passed, warned, failed)
	if failed > 0 {
		return fmt.Errorf("doctor found %d issue(s)", failed)
	}
	return nil
}

func checkConfig(record func(checkResult)) (*config.Config, error) {
	// Check for config file existence
	configFound := false
	configName := ""
	for _, name := range []string{"tako.yaml", "tako.yml", "tako.json"} {
		if _, err := os.Stat(name); err == nil {
			configFound = true
			configName = name
			break
		}
	}

	if !configFound {
		record(checkResult{"FAIL", "Config file: Not found", "Create tako.yaml in project root"})
		return nil, fmt.Errorf("no config")
	}

	record(checkResult{"PASS", fmt.Sprintf("Config file: Found %s", configName), ""})

	// Try loading config
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		record(checkResult{"FAIL", fmt.Sprintf("Config parse: %v", err), "Fix syntax errors in config file"})
		return nil, err
	}

	record(checkResult{"PASS", fmt.Sprintf("Config parse: Valid (project: %s)", cfg.Project.Name), ""})
	return cfg, nil
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

	for _, serverName := range servers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			record(checkResult{"FAIL", fmt.Sprintf("%s: Not found in config", serverName), ""})
			continue
		}

		client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
			Host:     server.Host,
			Port:     server.Port,
			User:     server.User,
			SSHKey:   server.SSHKey,
			Password: server.Password,
		})
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s (%s): %v", serverName, server.Host, err), "Check SSH key and server config"})
			continue
		}

		if err := client.Connect(); err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s (%s): Connection failed — %v", serverName, server.Host, err), "Check server is running and SSH access is configured"})
			continue
		}

		// Verify connectivity with a simple command
		output, err := client.Execute("echo 'tako-ok'")
		if err != nil {
			record(checkResult{"FAIL", fmt.Sprintf("%s (%s): Connected but command failed — %v", serverName, server.Host, err), ""})
			client.Close()
			continue
		}

		if strings.TrimSpace(output) != "tako-ok" {
			record(checkResult{"WARN", fmt.Sprintf("%s (%s): Connected but unexpected output", serverName, server.Host), ""})
		} else {
			record(checkResult{"PASS", fmt.Sprintf("%s (%s): Connected", serverName, server.Host), ""})
		}
		clients[serverName] = client
	}

	return clients
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
