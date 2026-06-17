package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/provisioner"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
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

		fmt.Println("\n=== Docker Runtime ===")
		checkDockerRuntime(record, clients)

		// === Running Services ===
		fmt.Println("\n=== Running Services ===")
		checkRunningServices(record, cfg, envName, clients)

		fmt.Println("\n=== External Volumes ===")
		checkExternalVolumes(record, cfg, envName, clients)

		// Clean up clients
		for _, c := range clients {
			c.Close()
		}
	} else {
		fmt.Println("\n=== Server Connectivity ===")
		fmt.Println("  [SKIP] Skipped (--skip-remote)")
		fmt.Println("\n=== Docker Runtime ===")
		fmt.Println("  [SKIP] Skipped (--skip-remote)")
		fmt.Println("\n=== Running Services ===")
		fmt.Println("  [SKIP] Skipped (--skip-remote)")
		fmt.Println("\n=== External Volumes ===")
		fmt.Println("  [SKIP] Skipped (--skip-remote)")
	}

	// Summary
	fmt.Printf("\nSummary: %d passed, %d warning(s), %d failed\n", passed, warned, failed)
	if failed > 0 {
		return fmt.Errorf("doctor found %d issue(s)", failed)
	}
	return nil
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
		return "Set SERVER_HOST in .env or replace ${SERVER_HOST} in config"
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
