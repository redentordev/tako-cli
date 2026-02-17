package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/secrets"
	"github.com/redentordev/tako-cli/pkg/ssh"
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

func runCloneSetup(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)
	passed, warned, failed := 0, 0, 0

	printStep := func(num int, title string) {
		fmt.Printf("\n=== Step %d: %s ===\n", num, title)
	}

	pass := func(msg string) {
		passed++
		fmt.Printf("  [PASS] %s\n", msg)
	}
	warn := func(msg string) {
		warned++
		fmt.Printf("  [WARN] %s\n", msg)
	}
	fail := func(msg string) {
		failed++
		fmt.Printf("  [FAIL] %s\n", msg)
	}

	isInteractive := term.IsTerminal(int(os.Stdin.Fd()))

	// Step 1: Check config
	printStep(1, "Configuration")

	cfg, err := config.LoadConfigWithInfra(cfgFile, ".tako")
	if err != nil {
		// Try without infra
		cfg, err = config.LoadConfig(cfgFile)
		if err != nil {
			fail(fmt.Sprintf("Cannot load config: %v", err))
			fmt.Println("\n  Make sure you're in the project root with a valid tako.yaml or tako.json.")
			return nil
		}
	}
	pass(fmt.Sprintf("Config loaded: %s (project: %s)", cfgFile, cfg.Project.Name))

	envName := getEnvironmentName(cfg)

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
			fmt.Println("  Fix: cp .env.example .env && edit .env")
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
		// Pick a connected client (prefer manager)
		var pullClient *ssh.Client
		managerName, err := cfg.GetManagerServer(envName)
		if err == nil {
			if c, ok := connectedClients[managerName]; ok {
				pullClient = c
			}
		}
		if pullClient == nil {
			for _, c := range connectedClients {
				pullClient = c
				break
			}
		}

		// Check if env.enc exists on server
		remotePath := fmt.Sprintf("%s/%s/env.enc", remotestate.StateDir, cfg.Project.Name)
		checkCmd := fmt.Sprintf("test -f %s && echo 'exists' || echo 'missing'", remotePath)
		output, err := pullClient.Execute(checkCmd)
		if err == nil && strings.TrimSpace(output) == "exists" {
			pass("Encrypted environment bundle found on server")
			if isInteractive {
				fmt.Print("  Pull and decrypt environment files? [Y/n] ")
				input, _ := reader.ReadString('\n')
				if answer := strings.TrimSpace(input); answer == "" || strings.ToLower(answer) == "y" {
					fmt.Println("  Run: tako env pull")
					fmt.Println("  (Requires the passphrase used during 'tako env push')")
				}
			}
		} else {
			warn("No encrypted environment bundle on server (run 'tako env push' to create one)")
		}
	} else {
		warn("No connected servers — cannot check for environment bundle")
	}

	// Step 5: State pull
	printStep(5, "Deployment State")

	if _, err := os.Stat(".tako"); err == nil {
		pass(".tako directory exists")
	} else if len(connectedClients) > 0 {
		warn(".tako directory missing, attempting state pull...")
		if err := SyncStateOnDeploy(cfg, getFirstClient(connectedClients, cfg, envName), envName); err != nil {
			warn(fmt.Sprintf("State sync failed: %v", err))
		} else {
			if _, err := os.Stat(".tako"); err == nil {
				pass("State synced from remote server")
			} else {
				warn("No remote state available (deploy first)")
			}
		}
	} else {
		warn(".tako directory missing and no server connections available")
		fmt.Println("  Fix: tako state pull")
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
					fmt.Println("  Fix: tako secrets set KEY=VALUE")
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
	fmt.Println("\n=== Summary ===")
	fmt.Printf("  %d passed, %d warning(s), %d failed\n", passed, warned, failed)

	if failed > 0 || warned > 0 {
		fmt.Println("\nNext steps:")
		if failed > 0 {
			fmt.Println("  - Fix failed checks above before deploying")
		}
		if warned > 0 {
			fmt.Println("  - Address warnings for full functionality")
		}
		fmt.Println("  - Run 'tako doctor' for detailed diagnostics")
	} else {
		fmt.Println("\nYour project is ready! Run 'tako deploy' to deploy.")
	}

	return nil
}

// createEnvFromExample creates .env from .env.example, optionally prompting for values
func createEnvFromExample(reader *bufio.Reader, interactive bool) error {
	exampleData, err := os.ReadFile(".env.example")
	if err != nil {
		return err
	}

	if !interactive {
		return os.WriteFile(".env", exampleData, 0600)
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

	return os.WriteFile(".env", []byte(strings.Join(outputLines, "\n")), 0600)
}

// getFirstClient returns a connected client, preferring the manager
func getFirstClient(clients map[string]*ssh.Client, cfg *config.Config, envName string) *ssh.Client {
	managerName, err := cfg.GetManagerServer(envName)
	if err == nil {
		if c, ok := clients[managerName]; ok {
			return c
		}
	}
	for _, c := range clients {
		return c
	}
	return nil
}
