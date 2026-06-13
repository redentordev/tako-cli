package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/crypto"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var envForce bool

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage environment credentials on remote servers",
	Long: `Push and pull encrypted environment files (.env, secrets) to/from remote servers.

This allows you to securely transfer credentials between machines. Files are
encrypted with a passphrase using Argon2id + AES-256-GCM before upload.

Examples:
  tako env push              # Encrypt and upload .env + secrets
  tako env pull              # Download and decrypt to local
  tako env pull --force      # Overwrite existing local files`,
}

var envPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Encrypt and upload environment files to server",
	Long: `Encrypt .env and .tako/secrets* files with a passphrase and upload
the encrypted bundle to the remote server.

The encrypted bundle is stored in takod state on the selected node,
protected by your passphrase.`,
	RunE: runEnvPush,
}

var envPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Download and decrypt environment files from server",
	Long: `Download the encrypted environment bundle from the remote server,
decrypt it with your passphrase, and restore the files locally.

By default, refuses to overwrite existing files. Use --force to override.`,
	RunE: runEnvPull,
}

func init() {
	rootCmd.AddCommand(envCmd)
	envCmd.AddCommand(envPushCmd)
	envCmd.AddCommand(envPullCmd)

	envPullCmd.Flags().BoolVar(&envForce, "force", false, "Overwrite existing local files")
}

func runEnvPush(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	// Collect files to bundle
	bundle := make(map[string]string) // relative path -> base64 content

	// Add .env if it exists
	if data, err := os.ReadFile(".env"); err == nil {
		bundle[".env"] = base64.StdEncoding.EncodeToString(data)
		fmt.Println("Including: .env")
	}

	// Add .tako/secrets* files
	matches, _ := filepath.Glob(".tako/secrets*")
	for _, match := range matches {
		data, err := os.ReadFile(match)
		if err != nil {
			fmt.Printf("Warning: skipping %s: %v\n", match, err)
			continue
		}
		bundle[match] = base64.StdEncoding.EncodeToString(data)
		fmt.Printf("Including: %s\n", match)
	}

	if len(bundle) == 0 {
		return fmt.Errorf("no environment files found to push (.env, .tako/secrets*)")
	}

	// Prompt for passphrase
	passphrase, err := promptPassphraseConfirm()
	if err != nil {
		return err
	}

	// Bundle into JSON
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return fmt.Errorf("failed to create bundle: %w", err)
	}

	// Encrypt with passphrase
	encrypted, err := crypto.EncryptWithPassphrase(bundleJSON, passphrase)
	if err != nil {
		return fmt.Errorf("failed to encrypt bundle: %w", err)
	}

	fmt.Printf("\nEncrypted %d file(s), uploading...\n", len(bundle))

	// Connect to server
	serverName, serverCfg, err := resolveServer(cfg, envName, "")
	if err != nil {
		return err
	}

	client, err := ssh.NewClientWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	defer client.Close()

	request := takod.EnvBundleRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Content:     base64.StdEncoding.EncodeToString(encrypted),
	}
	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "PUT", "/v1/env-bundle", request)
	if err != nil {
		return fmt.Errorf("failed to upload environment bundle through takod: %w", err)
	}
	var response takod.EnvBundleResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return err
	}

	fmt.Printf("Environment files pushed to takod state on %s\n", serverName)
	fmt.Println("\nTo restore on another machine:")
	fmt.Println("  tako env pull")
	return nil
}

func runEnvPull(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	envName := getEnvironmentName(cfg)

	// Connect to server
	serverName, serverCfg, err := resolveServer(cfg, envName, "")
	if err != nil {
		return err
	}

	client, err := ssh.NewClientWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	defer client.Close()

	output, err := takodclient.RequestJSON(
		client,
		takodSocketFromConfig(cfg),
		"GET",
		takodclient.EnvBundleEndpoint(cfg.Project.Name, envName),
		nil,
	)
	if err != nil {
		return fmt.Errorf("failed to download environment bundle through takod: %w", err)
	}

	var response takod.EnvBundleResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return err
	}
	if !response.Found {
		fmt.Println("No environment bundle found on server.")
		fmt.Println("Run 'tako env push' to upload environment files first.")
		return nil
	}

	encrypted, err := base64.StdEncoding.DecodeString(strings.TrimSpace(response.Content))
	if err != nil {
		return fmt.Errorf("failed to decode downloaded bundle: %w", err)
	}

	// Prompt for passphrase
	passphrase, err := promptPassphrase("Enter passphrase: ")
	if err != nil {
		return err
	}

	// Decrypt
	bundleJSON, err := crypto.DecryptWithPassphrase(encrypted, passphrase)
	if err != nil {
		return fmt.Errorf("failed to decrypt bundle: %w", err)
	}

	// Parse bundle
	var bundle map[string]string
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return fmt.Errorf("failed to parse bundle: %w", err)
	}

	// Check for existing files
	if !envForce {
		var existing []string
		for path := range bundle {
			if _, err := os.Stat(path); err == nil {
				existing = append(existing, path)
			}
		}
		if len(existing) > 0 {
			fmt.Println("The following files already exist locally:")
			for _, path := range existing {
				fmt.Printf("  - %s\n", path)
			}
			fmt.Println("\nUse --force to overwrite existing files.")
			return nil
		}
	}

	// Write files
	for path, encodedContent := range bundle {
		content, err := base64.StdEncoding.DecodeString(encodedContent)
		if err != nil {
			fmt.Printf("Warning: failed to decode %s: %v\n", path, err)
			continue
		}

		// Ensure directory exists
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				fmt.Printf("Warning: failed to create directory for %s: %v\n", path, err)
				continue
			}
		}

		if err := os.WriteFile(path, content, 0600); err != nil {
			fmt.Printf("Warning: failed to write %s: %v\n", path, err)
			continue
		}

		fmt.Printf("Restored: %s\n", path)
	}

	fmt.Printf("\nRestored %d file(s) from server\n", len(bundle))
	return nil
}

// promptPassphrase reads a passphrase from the terminal without echo
func promptPassphrase(prompt string) (string, error) {
	fmt.Print(prompt)
	passBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println() // newline after hidden input
	if err != nil {
		return "", fmt.Errorf("failed to read passphrase: %w", err)
	}

	passphrase := string(passBytes)
	if len(passphrase) < 8 {
		return "", fmt.Errorf("passphrase must be at least 8 characters")
	}

	return passphrase, nil
}

// promptPassphraseConfirm prompts for a passphrase with confirmation
func promptPassphraseConfirm() (string, error) {
	passphrase, err := promptPassphrase("Enter passphrase for encryption: ")
	if err != nil {
		return "", err
	}

	confirm, err := promptPassphrase("Confirm passphrase: ")
	if err != nil {
		return "", err
	}

	if passphrase != confirm {
		return "", fmt.Errorf("passphrases do not match")
	}

	return passphrase, nil
}
