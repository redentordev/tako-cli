package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
)

var proxyCmd = &cobra.Command{
	Use:   "proxy",
	Short: "Proxy access-control helpers",
}

var proxyHashPasswordCost int

var proxyHashPasswordCmd = &cobra.Command{
	Use:          "hash-password",
	Short:        "Mint a bcrypt hash for proxy.basicAuth.passwordBcrypt",
	SilenceUsage: true,
	Long: `Hash a password for proxy basic authentication.

The config takes a pre-computed bcrypt hash instead of a plaintext password so
the hash stays stable across deploys (bcrypt salts fresh hashes differently on
every call, which would churn proxy state). Reads the password from the
terminal without echo, or from stdin when piped.`,
	Example: `  tako proxy hash-password
  echo -n "s3cret" | tako proxy hash-password
  tako proxy hash-password --output json`,
	Args: cobra.NoArgs,
	RunE: runProxyHashPassword,
}

func init() {
	rootCmd.AddCommand(proxyCmd)
	proxyCmd.AddCommand(proxyHashPasswordCmd)
	proxyHashPasswordCmd.Flags().IntVar(&proxyHashPasswordCost, "cost", bcrypt.DefaultCost, "bcrypt cost factor")
}

func runProxyHashPassword(cmd *cobra.Command, args []string) error {
	if proxyHashPasswordCost < bcrypt.MinCost || proxyHashPasswordCost > bcrypt.MaxCost {
		return &engine.InvalidRequestError{Err: fmt.Errorf("--cost must be between %d and %d", bcrypt.MinCost, bcrypt.MaxCost)}
	}
	password, err := readProxyHashPasswordInput(cmd)
	if err != nil {
		return &engine.InvalidRequestError{Err: err}
	}
	cliEngine().RegisterSecret(password)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), proxyHashPasswordCost)
	if err != nil {
		return fmt.Errorf("failed to hash password: %w", err)
	}

	result := engine.ProxyHashPasswordResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindProxyHashPasswordResult,
		Cost:       proxyHashPasswordCost,
		Hash:       string(hash),
	}
	if !machineOutputEnabled() {
		fmt.Fprintln(cmd.OutOrStdout(), string(hash))
		fmt.Fprintln(os.Stderr, "Set this as proxy.basicAuth.passwordBcrypt in tako.yaml.")
	}
	return emitResultDocument(result)
}

// readProxyHashPasswordInput reads the password without echo from an
// interactive terminal, or verbatim from piped stdin (one trailing newline
// stripped). There is deliberately no --password flag: argv is visible in
// process listings.
func readProxyHashPasswordInput(cmd *cobra.Command) (string, error) {
	var password string
	if stdinFile, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(stdinFile.Fd())) {
		if machineOutputEnabled() {
			return "", fmt.Errorf("machine output requires the password on stdin (pipe it in)")
		}
		fmt.Fprint(os.Stderr, "Password: ")
		first, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("failed to read password: %w", err)
		}
		fmt.Fprint(os.Stderr, "Confirm password: ")
		second, err := term.ReadPassword(int(syscall.Stdin))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("failed to read password confirmation: %w", err)
		}
		if string(first) != string(second) {
			return "", fmt.Errorf("passwords do not match")
		}
		password = string(first)
	} else {
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("failed to read password from stdin: %w", err)
		}
		password = strings.TrimSuffix(string(data), "\n")
		password = strings.TrimSuffix(password, "\r")
	}
	if password == "" {
		return "", fmt.Errorf("password is empty")
	}
	if len(password) > 72 {
		return "", fmt.Errorf("password is longer than 72 bytes, the bcrypt maximum")
	}
	return password, nil
}
