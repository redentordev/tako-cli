package cmd

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var cloneSetupCmd = &cobra.Command{
	Use:   "clone-setup",
	Short: "Set up a cloned project on a new machine",
	Long: `Interactive walkthrough for setting up a Tako project after cloning
to a new machine.

This command guides you through:
  1. Verifying the configuration file
  2. Setting up .env from .env.example
  3. Testing SSH connectivity to servers
  4. Pulling encrypted environment files (if available)
  5. Syncing deployment state from remote servers
  6. Validating secrets

Examples:
  tako clone-setup              # Run guided setup
`,
	RunE: runCloneSetup,
}

func init() {
	rootCmd.AddCommand(cloneSetupCmd)
}

var cloneSetupCollectStatePullHistories = collectStatePullHistories
var cloneSetupSyncBestDeploymentHistoryToLocal = syncBestDeploymentHistoryToLocal
var cloneSetupRecoverStateFromMeshActual = recoverAndSaveStateFromMeshActual

func runCloneSetup(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)
	passed, warned, failed := 0, 0, 0
	result := engine.CloneSetupResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindCloneSetupResult,
		Checks:     []engine.DoctorCheck{},
	}

	// Machine modes reserve stdout for parseable output.
	out := humanOut()

	section := ""
	printStep := func(num int, title string) {
		section = title
		fmt.Fprintf(out, "\n=== Step %d: %s ===\n", num, title)
	}

	record := func(status string, msg string) {
		result.Checks = append(result.Checks, engine.DoctorCheck{Name: section, Status: status, Detail: msg})
		fmt.Fprintf(out, "  [%s] %s\n", strings.ToUpper(status), msg)
	}
	pass := func(msg string) {
		passed++
		record(engine.DoctorStatusPass, msg)
	}
	warn := func(msg string) {
		warned++
		record(engine.DoctorStatusWarn, msg)
	}
	fail := func(msg string) {
		failed++
		record(engine.DoctorStatusFail, msg)
	}

	finish := func() error {
		result.Passed, result.Warned, result.Failed = passed, warned, failed
		result.Status = "ok"
		if failed > 0 {
			result.Status = "attention"
		}
		if err := emitResultDocument(result); err != nil {
			return err
		}
		if failed > 0 {
			return &engine.AttentionError{Err: fmt.Errorf("clone-setup found %d issue(s)", failed)}
		}
		return nil
	}

	// Machine modes must not block on interactive fix-up prompts.
	isInteractive := term.IsTerminal(int(os.Stdin.Fd())) && !machineOutputEnabled()

	// Step 1: Check config
	printStep(1, "Configuration")

	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		fail(fmt.Sprintf("Cannot load config: %v", err))
		fmt.Fprintln(out, "\n  Make sure you're in the project root with a valid tako.yaml or tako.json.")
		return finish()
	}
	result.Project = cfg.Project.Name
	pass(fmt.Sprintf("Config loaded: %s (project: %s)", cfgFile, cfg.Project.Name))

	envName := getEnvironmentName(cfg)
	result.Environment = envName

	// Step 2: Check .env
	printStep(2, "Environment File")

	if _, err := os.Stat(".env"); err == nil {
		pass(".env file exists")
	} else if _, err := os.Stat(".env.example"); err == nil {
		warn(".env file missing, .env.example found")

		if isInteractive {
			fmt.Print("  Create .env from .env.example? [Y/n] ")
			input, _ := reader.ReadString('\n')
			if answer := strings.TrimSpace(input); answer == "" || strings.ToLower(answer) == "y" {
				if err := createEnvFromExample(reader, isInteractive); err != nil {
					warn(fmt.Sprintf("Failed to create .env: %v", err))
				} else {
					pass(".env created from .env.example")
				}
			}
		} else {
			fmt.Fprintln(out, "  Fix: cp .env.example .env && edit .env")
		}
	} else {
		warn(".env file missing (no .env.example either)")
	}

	// Step 3: Test SSH
	printStep(3, "SSH Connectivity")

	servers, err := cfg.GetEnvironmentServers(envName)
	if err != nil {
		warn(fmt.Sprintf("Cannot resolve servers: %v", err))
		servers = nil
	}

	connectedClients := make(map[string]*ssh.Client)
	for _, serverName := range servers {
		server, ok := cfg.Servers[serverName]
		if !ok {
			fail(fmt.Sprintf("%s: Server not found in config", serverName))
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
			fail(fmt.Sprintf("%s (%s): %v", serverName, server.Host, err))

			if isInteractive {
				fmt.Print("  Enter alternative SSH key path (or press Enter to skip): ")
				input, _ := reader.ReadString('\n')
				altKey := strings.TrimSpace(input)
				if altKey != "" {
					client, err = ssh.NewClientFromConfig(ssh.ServerConfig{
						Host:     server.Host,
						Port:     server.Port,
						User:     server.User,
						SSHKey:   altKey,
						Password: server.Password,
					})
					if err != nil {
						fail(fmt.Sprintf("%s: Alternative key also failed: %v", serverName, err))
						continue
					}
				} else {
					continue
				}
			} else {
				continue
			}
		}

		if err := client.Connect(); err != nil {
			fail(fmt.Sprintf("%s (%s): Connection failed — %v", serverName, server.Host, err))
			continue
		}

		pass(fmt.Sprintf("%s (%s): Connected", serverName, server.Host))
		connectedClients[serverName] = client
	}

	// Step 4: Try env pull
	printStep(4, "Environment Pull")

	if len(connectedClients) > 0 {
		connectedNames := make([]string, 0, len(connectedClients))
		for name := range connectedClients {
			connectedNames = append(connectedNames, name)
		}
		sort.Strings(connectedNames)
		candidates := make([]envBundleDownloadCandidate, 0, len(connectedNames))
		for index, name := range connectedNames {
			output, err := takodclient.RequestJSON(
				connectedClients[name],
				takodSocketFromConfig(cfg),
				"GET",
				takodclient.EnvBundleEndpoint(cfg.Project.Name, envName),
				nil,
			)
			if err != nil {
				continue
			}
			var candidate takod.EnvBundleResponse
			if err := decodeTakodJSON(output, &candidate); err != nil {
				continue
			}
			if candidate.Found {
				if candidate.UpdatedAt.IsZero() {
					warn(fmt.Sprintf("%s: environment bundle missing updatedAt metadata", name))
					continue
				}
				candidates = append(candidates, envBundleDownloadCandidate{
					response: &candidate,
					source:   name,
					index:    index,
				})
			}
		}

		if len(candidates) > 0 {
			selected := selectFreshestEnvBundleCandidate(candidates)
			response := selected.response
			bundleSource := selected.source
			pass(fmt.Sprintf("Encrypted environment bundle found on %s", bundleSource))
			if isInteractive {
				fmt.Print("  Pull and decrypt environment files? [Y/n] ")
				input, _ := reader.ReadString('\n')
				if answer := strings.TrimSpace(input); answer == "" || strings.ToLower(answer) == "y" {
					restored, skipped, err := restoreDownloadedEnvBundle(response, false, cfg)
					if err != nil {
						warn(fmt.Sprintf("Environment pull failed: %v", err))
					} else if skipped {
						warn("Environment files already exist; run 'tako env pull --force' to overwrite")
					} else {
						pass(fmt.Sprintf("Restored %d environment file(s) from %s", restored, bundleSource))
					}
				}
			}
		} else {
			warn("No encrypted environment bundle on reachable servers (run 'tako env push' to create one)")
		}
	} else {
		warn("No connected servers — cannot check for environment bundle")
	}

	// Step 5: State pull
	printStep(5, "Deployment State")

	if localDeploymentStateExists(envName) {
		pass("Local deployment state exists")
	} else if len(connectedClients) > 0 {
		warn("Local deployment state missing, attempting state pull...")
		message, err := cloneSetupSyncMissingState(cfg, envName)
		if err != nil {
			warn(err.Error())
		} else {
			pass(message)
		}
	} else {
		warn("Local deployment state missing and no server connections available")
		fmt.Fprintln(out, "  Fix: tako state pull")
	}

	// Step 6: Validate secrets
	printStep(6, "Secrets")

	services, err := cfg.GetServices(envName)
	if err == nil {
		var required []string
		for _, svc := range services {
			required = append(required, svc.Secrets...)
		}

		if len(required) == 0 {
			pass("No secrets required by services")
		} else {
			mgr, err := secrets.NewManager(envName)
			if err != nil {
				warn(fmt.Sprintf("Cannot initialize secrets manager: %v", err))
			} else {
				if err := mgr.ValidateRequired(required); err != nil {
					warn(fmt.Sprintf("Missing secrets: %v", err))
					fmt.Fprintln(out, "  Fix: tako secrets set KEY=VALUE")
				} else {
					pass(fmt.Sprintf("All %d required secret(s) present", len(required)))
				}
			}
		}
	}

	// Clean up SSH clients
	for _, c := range connectedClients {
		c.Close()
	}

	// Summary
	fmt.Fprintln(out, "\n=== Summary ===")
	fmt.Fprintf(out, "  %d passed, %d warning(s), %d failed\n", passed, warned, failed)

	if failed > 0 || warned > 0 {
		fmt.Fprintln(out, "\nNext steps:")
		if failed > 0 {
			fmt.Fprintln(out, "  - Fix failed checks above before deploying")
		}
		if warned > 0 {
			fmt.Fprintln(out, "  - Address warnings for full functionality")
		}
		fmt.Fprintln(out, "  - Run 'tako doctor' for detailed diagnostics")
	} else {
		fmt.Fprintln(out, "\nYour project is ready! Run 'tako deploy' to deploy.")
	}

	return finish()
}

func cloneSetupSyncMissingState(cfg *config.Config, envName string) (string, error) {
	histories, err := cloneSetupCollectStatePullHistories(cfg, envName, "")
	if err != nil {
		return "", fmt.Errorf("State sync failed: %w", err)
	}
	source, synced, ok, err := cloneSetupSyncBestDeploymentHistoryToLocal(cfg, envName, histories)
	if err != nil {
		return "", fmt.Errorf("State sync failed: %w", err)
	}
	if ok {
		return fmt.Sprintf("State synced from %s (%d deployment(s))", source, synced), nil
	}
	if localDeploymentStateExists(envName) {
		return "State synced from remote mesh", nil
	}
	if err := cloneSetupRecoverStateFromMeshActual(cfg, envName, ""); err != nil {
		return "", fmt.Errorf("No remote state available (deploy first): %w", err)
	}
	return "State recovered from remote mesh runtime", nil
}

// createEnvFromExample creates .env from .env.example, optionally prompting for values
func createEnvFromExample(reader *bufio.Reader, interactive bool) error {
	exampleData, err := os.ReadFile(".env.example")
	if err != nil {
		return err
	}

	if !interactive {
		return fileutil.WriteFileAtomic(".env", exampleData, 0600)
	}

	// Parse .env.example and prompt for values
	lines := strings.Split(string(exampleData), "\n")
	var outputLines []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Pass through comments and empty lines
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			outputLines = append(outputLines, line)
			continue
		}

		// Parse KEY=VALUE
		parts := strings.SplitN(trimmed, "=", 2)
		if len(parts) != 2 {
			outputLines = append(outputLines, line)
			continue
		}

		key := parts[0]
		defaultVal := parts[1]

		fmt.Printf("  %s [%s]: ", key, defaultVal)
		input, _ := reader.ReadString('\n')
		val := strings.TrimSpace(input)
		if val == "" {
			val = defaultVal
		}

		outputLines = append(outputLines, fmt.Sprintf("%s=%s", key, val))
	}

	return fileutil.WriteFileAtomic(".env", []byte(strings.Join(outputLines, "\n")), 0600)
}
