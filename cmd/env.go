package cmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/crypto"
	"github.com/redentordev/tako-cli/pkg/fileutil"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var envForce bool

const envPassphraseVar = "TAKO_ENV_PASSPHRASE"

var downloadEnvBundleFromServerFunc = downloadEnvBundleFromServer
var uploadEnvBundleToServerFunc = uploadEnvBundleToServer

var envCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage environment credentials in the takod mesh",
	Long: `Push and pull encrypted environment files (.env, secrets) to/from the takod mesh.

This allows you to securely transfer credentials between machines. Files are
encrypted with a passphrase using Argon2id + AES-256-GCM before upload.

Examples:
  tako env push              # Encrypt and upload .env + secrets
  tako env pull              # Download and decrypt to local
  tako env pull --force      # Overwrite existing local files

For CI, set TAKO_ENV_PASSPHRASE to avoid interactive prompts.`,
}

var envPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Encrypt and upload environment files to the takod mesh",
	Long: `Encrypt .env and .tako/secrets* files with a passphrase and upload
the encrypted bundle to reachable environment nodes.

The encrypted bundle is stored in takod state on each reachable node,
protected by your passphrase.`,
	RunE: runEnvPush,
}

var envPullCmd = &cobra.Command{
	Use:   "pull",
	Short: "Download and decrypt environment files from the takod mesh",
	Long: `Download the encrypted environment bundle from reachable environment nodes,
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
	if err := requireTakodRuntime(cfg); err != nil {
		return err
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

	serverNames, err := statePullServerNames(cfg, envName, "")
	if err != nil {
		return err
	}

	request := takod.EnvBundleRequest{
		Project:     cfg.Project.Name,
		Environment: envName,
		Content:     base64.StdEncoding.EncodeToString(encrypted),
	}

	uploaded, nodeErrors := uploadEnvBundleToMesh(cfg, serverNames, request)
	if uploaded == 0 {
		return fmt.Errorf("failed to upload environment bundle to any node: %s", strings.Join(nodeErrors, "; "))
	}
	if len(nodeErrors) > 0 {
		return fmt.Errorf("environment bundle uploaded to %d/%d node(s), failed on %s", uploaded, len(serverNames), strings.Join(nodeErrors, "; "))
	}

	fmt.Printf("Environment files pushed to takod state on %d node(s)\n", uploaded)
	fmt.Println("\nTo restore on another machine:")
	fmt.Println("  tako env pull")
	return nil
}

func runEnvPull(cmd *cobra.Command, args []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}

	envName := getEnvironmentName(cfg)

	response, source, err := downloadEnvBundleFromMesh(cfg, envName)
	if err != nil {
		return err
	}
	if response == nil || !response.Found {
		fmt.Println("No environment bundle found on reachable nodes.")
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

	allowedBundle, allowedPaths, skippedPaths := supportedEnvBundleFiles(bundle)
	for _, path := range skippedPaths {
		fmt.Printf("Warning: skipping unsupported bundle path: %s\n", path)
	}
	if len(allowedPaths) == 0 {
		return fmt.Errorf("environment bundle did not contain any supported files")
	}

	// Check for existing files
	if !envForce {
		var existing []string
		for _, path := range allowedPaths {
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

	restored, err := restoreEnvBundleFiles(allowedBundle, allowedPaths)
	if err != nil {
		return err
	}

	fmt.Printf("\nRestored %d file(s) from %s\n", restored, source)
	return nil
}

func restoreEnvBundleFiles(allowedBundle map[string]string, allowedPaths []string) (int, error) {
	decoded := make(map[string][]byte, len(allowedPaths))
	var decodeErrors []string
	for _, path := range allowedPaths {
		content, err := base64.StdEncoding.DecodeString(allowedBundle[path])
		if err != nil {
			decodeErrors = append(decodeErrors, fmt.Sprintf("%s: %v", path, err))
			continue
		}
		decoded[path] = content
	}
	if len(decodeErrors) > 0 {
		return 0, fmt.Errorf("failed to decode environment bundle file(s): %s", strings.Join(decodeErrors, "; "))
	}

	restored := 0
	var writeErrors []string
	for _, path := range allowedPaths {
		if dir := filepath.Dir(path); dir != "." {
			if err := os.MkdirAll(dir, 0755); err != nil {
				writeErrors = append(writeErrors, fmt.Sprintf("%s: create directory: %v", path, err))
				continue
			}
		}

		if err := fileutil.WriteFileAtomic(path, decoded[path], 0600); err != nil {
			writeErrors = append(writeErrors, fmt.Sprintf("%s: write: %v", path, err))
			continue
		}

		fmt.Printf("Restored: %s\n", path)
		restored++
	}
	if len(writeErrors) > 0 {
		return restored, fmt.Errorf("failed to restore environment bundle file(s): %s", strings.Join(writeErrors, "; "))
	}
	return restored, nil
}

func supportedEnvBundleFiles(bundle map[string]string) (map[string]string, []string, []string) {
	allowed := make(map[string]string, len(bundle))
	allowedPaths := make([]string, 0, len(bundle))
	skippedPaths := make([]string, 0)
	for path, encodedContent := range bundle {
		if !isAllowedEnvBundlePath(path) {
			skippedPaths = append(skippedPaths, path)
			continue
		}
		allowed[path] = encodedContent
		allowedPaths = append(allowedPaths, path)
	}
	sort.Strings(allowedPaths)
	sort.Strings(skippedPaths)
	return allowed, allowedPaths, skippedPaths
}

func uploadEnvBundleToServer(cfg *config.Config, serverName string, serverCfg config.ServerConfig, request takod.EnvBundleRequest) error {
	client, err := ssh.NewClientWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return fmt.Errorf("failed to connect to %s: %w", serverName, err)
	}
	defer client.Close()

	output, err := takodclient.RequestJSON(client, takodSocketFromConfig(cfg), "PUT", "/v1/env-bundle", request)
	if err != nil {
		return fmt.Errorf("failed to upload environment bundle through takod: %w", err)
	}
	var response takod.EnvBundleResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return err
	}
	if !response.Found {
		return fmt.Errorf("takod did not confirm environment bundle write")
	}
	return nil
}

type envBundleUploadResult struct {
	index      int
	serverName string
	err        error
}

func uploadEnvBundleToMesh(cfg *config.Config, serverNames []string, request takod.EnvBundleRequest) (int, []string) {
	resultCh := make(chan envBundleUploadResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		serverCfg, ok := cfg.Servers[serverName]
		if !ok {
			resultCh <- envBundleUploadResult{
				index:      index,
				serverName: serverName,
				err:        fmt.Errorf("server not found in configuration"),
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, serverCfg config.ServerConfig) {
			defer wg.Done()
			resultCh <- envBundleUploadResult{
				index:      index,
				serverName: serverName,
				err:        uploadEnvBundleToServerFunc(cfg, serverName, serverCfg, request),
			}
		}(index, serverName, serverCfg)
	}

	wg.Wait()
	close(resultCh)

	results := make([]envBundleUploadResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}

	uploaded := 0
	var nodeErrors []string
	for _, result := range results {
		if result.err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			continue
		}
		uploaded++
	}
	return uploaded, nodeErrors
}

func downloadEnvBundleFromMesh(cfg *config.Config, envName string) (*takod.EnvBundleResponse, string, error) {
	serverNames, err := statePullServerNames(cfg, envName, "")
	if err != nil {
		return nil, "", err
	}

	var nodeErrors []string
	for _, serverName := range serverNames {
		serverCfg, err := serverConfigByName(cfg, serverName)
		if err != nil {
			return nil, "", err
		}
		response, err := downloadEnvBundleFromServerFunc(cfg, envName, serverName, serverCfg)
		if err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", serverName, err))
			continue
		}
		if response != nil && response.Found {
			return response, serverName, nil
		}
	}
	if len(nodeErrors) == len(serverNames) {
		return nil, "", fmt.Errorf("failed to read environment bundle from any node: %s", strings.Join(nodeErrors, "; "))
	}
	return &takod.EnvBundleResponse{Found: false}, "", nil
}

func downloadEnvBundleFromServer(cfg *config.Config, envName string, serverName string, serverCfg config.ServerConfig) (*takod.EnvBundleResponse, error) {
	client, err := ssh.NewClientWithAuth(serverCfg.Host, serverCfg.Port, serverCfg.User, serverCfg.SSHKey, serverCfg.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s: %w", serverName, err)
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
		return nil, fmt.Errorf("failed to download environment bundle through takod: %w", err)
	}

	var response takod.EnvBundleResponse
	if err := decodeTakodJSON(output, &response); err != nil {
		return nil, err
	}
	return &response, nil
}

// promptPassphrase reads a passphrase from the terminal without echo
func promptPassphrase(prompt string) (string, error) {
	if passphrase, ok, err := passphraseFromEnv(); ok || err != nil {
		return passphrase, err
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("passphrase prompt requires a terminal; set %s for non-interactive use", envPassphraseVar)
	}

	fmt.Print(prompt)
	passBytes, err := term.ReadPassword(int(syscall.Stdin))
	fmt.Println() // newline after hidden input
	if err != nil {
		return "", fmt.Errorf("failed to read passphrase: %w", err)
	}

	passphrase := string(passBytes)
	if err := validateEnvPassphrase(passphrase); err != nil {
		return "", err
	}

	return passphrase, nil
}

// promptPassphraseConfirm prompts for a passphrase with confirmation
func promptPassphraseConfirm() (string, error) {
	if passphrase, ok, err := passphraseFromEnv(); ok || err != nil {
		return passphrase, err
	}

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

func passphraseFromEnv() (string, bool, error) {
	passphrase := os.Getenv(envPassphraseVar)
	if passphrase == "" {
		return "", false, nil
	}
	if err := validateEnvPassphrase(passphrase); err != nil {
		return "", true, fmt.Errorf("%s: %w", envPassphraseVar, err)
	}
	return passphrase, true, nil
}

func validateEnvPassphrase(passphrase string) error {
	if len(passphrase) < 8 {
		return fmt.Errorf("passphrase must be at least 8 characters")
	}
	return nil
}

func isAllowedEnvBundlePath(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if path == ".env" {
		return true
	}
	if filepath.Dir(path) != ".tako" {
		return false
	}
	name := filepath.Base(path)
	return name == "secrets" || strings.HasPrefix(name, "secrets.")
}
