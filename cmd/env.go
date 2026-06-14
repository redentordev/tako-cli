package cmd

import (
	"encoding/base64"
	"encoding/json"
	"errors"
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
var writeEnvBundleFileAtomic = fileutil.WriteFileAtomic

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
	sshPool := ssh.NewPool()
	defer sshPool.CloseAll()
	leaseSet, err := acquireRemoteOperationLeasesFunc(sshPool, cfg, envName, serverNames, "env-push")
	if err != nil {
		return err
	}
	defer leaseSet.Release(verbose)
	if verbose {
		fmt.Printf("→ Acquired remote env-push leases: %s\n", leaseSet.Summary())
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

	restored, skipped, err := restoreDownloadedEnvBundle(response, envForce)
	if err != nil {
		return err
	}
	if skipped {
		return nil
	}

	fmt.Printf("\nRestored %d file(s) from %s\n", restored, source)
	return nil
}

func restoreDownloadedEnvBundle(response *takod.EnvBundleResponse, force bool) (int, bool, error) {
	encrypted, err := base64.StdEncoding.DecodeString(strings.TrimSpace(response.Content))
	if err != nil {
		return 0, false, fmt.Errorf("failed to decode downloaded bundle: %w", err)
	}

	// Prompt for passphrase
	passphrase, err := promptPassphrase("Enter passphrase: ")
	if err != nil {
		return 0, false, err
	}

	// Decrypt
	bundleJSON, err := crypto.DecryptWithPassphrase(encrypted, passphrase)
	if err != nil {
		return 0, false, fmt.Errorf("failed to decrypt bundle: %w", err)
	}

	// Parse bundle
	var bundle map[string]string
	if err := json.Unmarshal(bundleJSON, &bundle); err != nil {
		return 0, false, fmt.Errorf("failed to parse bundle: %w", err)
	}

	allowedBundle, allowedPaths, skippedPaths := supportedEnvBundleFiles(bundle)
	for _, path := range skippedPaths {
		fmt.Printf("Warning: skipping unsupported bundle path: %s\n", path)
	}
	if len(allowedPaths) == 0 {
		return 0, false, fmt.Errorf("environment bundle did not contain any supported files")
	}

	// Check for existing files
	if !force {
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
			return 0, true, nil
		}
	}

	restored, err := restoreEnvBundleFiles(allowedBundle, allowedPaths)
	if err != nil {
		return restored, false, err
	}
	return restored, false, nil
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

	backups, createdDirs, err := prepareEnvRestoreTransaction(allowedPaths)
	if err != nil {
		return 0, err
	}
	for _, path := range allowedPaths {
		if err := writeEnvBundleFileAtomic(path, decoded[path], 0600); err != nil {
			message := fmt.Sprintf("%s: write: %v", path, err)
			if rollbackErr := rollbackEnvRestore(backups, createdDirs); rollbackErr != nil {
				return 0, fmt.Errorf("failed to restore environment bundle file(s): %s; rollback failed: %w", message, rollbackErr)
			}
			return 0, fmt.Errorf("failed to restore environment bundle file(s): %s", message)
		}
	}

	for _, path := range allowedPaths {
		fmt.Printf("Restored: %s\n", path)
	}
	return len(allowedPaths), nil
}

type envRestoreBackup struct {
	path    string
	exists  bool
	content []byte
	mode    os.FileMode
}

func prepareEnvRestoreTransaction(paths []string) (map[string]envRestoreBackup, []string, error) {
	createdDirs := make([]string, 0)
	for _, path := range paths {
		if !isAllowedEnvBundlePath(path) {
			return nil, nil, fmt.Errorf("%s: unsupported environment bundle path", path)
		}

		dir := filepath.Dir(path)
		if dir == "." {
			continue
		}

		dirExisted := true
		if _, err := os.Lstat(dir); os.IsNotExist(err) {
			dirExisted = false
		} else if err != nil {
			return nil, nil, fmt.Errorf("%s: inspect directory: %w", path, err)
		}

		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, nil, fmt.Errorf("%s: create directory: %w", path, err)
		}
		if !dirExisted {
			createdDirs = append(createdDirs, dir)
		}
		if err := validateEnvRestoreDirectory(dir); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	backups := make(map[string]envRestoreBackup, len(paths))
	for _, path := range paths {
		if err := validateEnvRestoreTarget(path); err != nil {
			return nil, nil, err
		}

		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			backups[path] = envRestoreBackup{path: path}
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%s: inspect file: %w", path, err)
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: backup existing file: %w", path, err)
		}
		backups[path] = envRestoreBackup{
			path:    path,
			exists:  true,
			content: data,
			mode:    info.Mode().Perm(),
		}
	}

	return backups, createdDirs, nil
}

func validateEnvRestoreTarget(path string) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := validateEnvRestoreDirectory(dir); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}

	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: inspect file: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s: refusing to overwrite symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s: refusing to overwrite non-regular file", path)
	}
	return nil
}

func validateEnvRestoreDirectory(dir string) error {
	clean := filepath.Clean(dir)
	if clean == "." {
		return nil
	}

	current := ""
	for _, part := range strings.Split(clean, string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}

		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect directory %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to restore through symlink directory %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("restore parent %s is not a directory", current)
		}
	}
	return nil
}

func rollbackEnvRestore(backups map[string]envRestoreBackup, createdDirs []string) error {
	paths := make([]string, 0, len(backups))
	for path := range backups {
		paths = append(paths, path)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))

	var rollbackErrors []string
	for _, path := range paths {
		backup := backups[path]
		if backup.exists {
			if err := writeEnvBundleFileAtomic(backup.path, backup.content, backup.mode); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: restore backup: %v", backup.path, err))
			}
			continue
		}

		if err := os.Remove(backup.path); err != nil && !os.IsNotExist(err) {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: remove restored file: %v", backup.path, err))
		}
	}

	for i := len(createdDirs) - 1; i >= 0; i-- {
		if err := os.Remove(createdDirs[i]); err != nil && !os.IsNotExist(err) {
			rollbackErrors = append(rollbackErrors, fmt.Sprintf("%s: remove created directory: %v", createdDirs[i], err))
		}
	}

	if len(rollbackErrors) > 0 {
		return errors.New(strings.Join(rollbackErrors, "; "))
	}
	return nil
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

type envBundleDownloadCandidate struct {
	response *takod.EnvBundleResponse
	source   string
	index    int
}

type envBundleDownloadResult struct {
	index      int
	serverName string
	response   *takod.EnvBundleResponse
	err        error
}

func downloadEnvBundleFromMesh(cfg *config.Config, envName string) (*takod.EnvBundleResponse, string, error) {
	serverNames, err := statePullServerNames(cfg, envName, "")
	if err != nil {
		return nil, "", err
	}

	resultCh := make(chan envBundleDownloadResult, len(serverNames))
	var wg sync.WaitGroup

	for index, serverName := range serverNames {
		serverCfg, err := serverConfigByName(cfg, serverName)
		if err != nil {
			resultCh <- envBundleDownloadResult{
				index:      index,
				serverName: serverName,
				err:        err,
			}
			continue
		}

		wg.Add(1)
		go func(index int, serverName string, serverCfg config.ServerConfig) {
			defer wg.Done()
			response, err := downloadEnvBundleFromServerFunc(cfg, envName, serverName, serverCfg)
			resultCh <- envBundleDownloadResult{
				index:      index,
				serverName: serverName,
				response:   response,
				err:        err,
			}
		}(index, serverName, serverCfg)
	}

	wg.Wait()
	close(resultCh)

	results := make([]envBundleDownloadResult, len(serverNames))
	for result := range resultCh {
		results[result.index] = result
	}

	var nodeErrors []string
	candidates := make([]envBundleDownloadCandidate, 0, len(serverNames))
	for _, result := range results {
		if result.err != nil {
			nodeErrors = append(nodeErrors, fmt.Sprintf("%s: %v", result.serverName, result.err))
			continue
		}
		if result.response != nil && result.response.Found {
			if result.response.UpdatedAt.IsZero() {
				nodeErrors = append(nodeErrors, fmt.Sprintf("%s: environment bundle missing updatedAt metadata", result.serverName))
				continue
			}
			candidates = append(candidates, envBundleDownloadCandidate{
				response: result.response,
				source:   result.serverName,
				index:    result.index,
			})
		}
	}
	if len(candidates) > 0 {
		selected := selectFreshestEnvBundleCandidate(candidates)
		return selected.response, selected.source, nil
	}
	if len(nodeErrors) == len(serverNames) {
		return nil, "", fmt.Errorf("failed to read environment bundle from any node: %s", strings.Join(nodeErrors, "; "))
	}
	return &takod.EnvBundleResponse{Found: false}, "", nil
}

func selectFreshestEnvBundleCandidate(candidates []envBundleDownloadCandidate) envBundleDownloadCandidate {
	sort.SliceStable(candidates, func(i, j int) bool {
		left := candidates[i].response.UpdatedAt
		right := candidates[j].response.UpdatedAt
		if left.IsZero() && right.IsZero() {
			return candidates[i].index < candidates[j].index
		}
		if left.IsZero() {
			return false
		}
		if right.IsZero() {
			return true
		}
		if left.Equal(right) {
			return candidates[i].index < candidates[j].index
		}
		return left.After(right)
	})
	return candidates[0]
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
