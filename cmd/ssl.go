package cmd

import (
	"fmt"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/ssl"
	"github.com/spf13/cobra"
)

var sslCmd = &cobra.Command{
	Use:   "ssl",
	Short: "Manage SSL certificates",
	Long:  `Manage SSL certificates for your deployed services.`,
}

var sslStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show SSL certificate status",
	Long:  `Show the status of SSL certificates including pending wildcard certificates.`,
	RunE:  runSSLStatus,
}

func init() {
	rootCmd.AddCommand(sslCmd)
	sslCmd.AddCommand(sslStatusCmd)
}

func runSSLStatus(cmd *cobra.Command, args []string) error {
	// Load configuration
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	// Get manager server
	managerServerName, err := cfg.GetManagerServer(envName)
	if err != nil {
		return fmt.Errorf("failed to get manager server: %w", err)
	}

	server, exists := cfg.Servers[managerServerName]
	if !exists {
		return fmt.Errorf("server '%s' not found in configuration", managerServerName)
	}

	// Connect to server
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()

	client, err := sshPool.GetOrCreateWithAuth(server.Host, server.Port, server.User, server.SSHKey, server.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to server: %w", err)
	}

	// Create SSL monitor
	monitor := ssl.NewMonitor(client, cfg.Project.Name, envName, nil, verbose)

	// Get status
	statuses, err := monitor.GetStatus()
	if err != nil && verbose {
		fmt.Printf("Warning: failed to get pending status: %v\n", err)
	}

	fmt.Printf("\nSSL Certificate Status for %s (%s)\n", cfg.Project.Name, envName)
	fmt.Println("─────────────────────────────────────────────────────────────────")

	if len(statuses) == 0 {
		fmt.Println("\nNo pending wildcard certificates.")
		fmt.Println("All configured domains are using HTTP-01 challenge (automatic).")
		return nil
	}

	fmt.Printf("\n%-30s %-15s %-10s %s\n", "Domain", "Status", "Attempts", "Last Check")
	fmt.Println("─────────────────────────────────────────────────────────────────")

	for _, status := range statuses {
		statusIcon := "⏳"
		statusText := "Pending DNS"
		if status.DNSVerified {
			statusIcon = "✓"
			statusText = "DNS Ready"
		}

		lastCheck := "Never"
		if !status.LastCheck.IsZero() {
			lastCheck = time.Since(status.LastCheck).Round(time.Second).String() + " ago"
		}

		fmt.Printf("%-30s %s %-13s %-10d %s\n",
			"*."+status.Domain,
			statusIcon,
			statusText,
			status.Attempts,
			lastCheck)

		// Show CNAME instruction for pending
		if !status.DNSVerified {
			fmt.Printf("  └─ CNAME: _acme-challenge.%s → %s\n", status.Domain, status.CNAMETarget)
		}
	}

	fmt.Println()

	// Check pending certificates
	issued, stillPending, errs := monitor.CheckPending()
	if len(errs) > 0 && verbose {
		fmt.Println("\nWarnings during certificate check:")
		for _, err := range errs {
			fmt.Printf("  - %v\n", err)
		}
	}

	if len(issued) > 0 {
		fmt.Println("✓ Newly verified domains:")
		for _, domain := range issued {
			fmt.Printf("  - %s\n", domain)
		}
		fmt.Println()
	}

	if len(stillPending) > 0 {
		fmt.Println("⏳ Still waiting for DNS propagation:")
		for _, domain := range stillPending {
			fmt.Printf("  - %s\n", domain)
		}
		fmt.Println("\nDNS propagation can take up to 48 hours depending on your DNS provider.")
		fmt.Println("Tako will automatically issue certificates once DNS propagates.")
	}

	return nil
}
