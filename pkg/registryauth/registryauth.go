// Package registryauth mints short-lived private registry credentials on the
// deploying machine for registries.<host> entries that declare a provider
// (ecr, gcr/artifact-registry, ghcr). Tokens come from the provider CLIs the
// operator already uses, live only in memory for one CLI process, and are
// registered with the output redactor before any caller sees them.
package registryauth

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/redentordev/tako-cli/internal/providercli"
	"github.com/redentordev/tako-cli/pkg/config"
)

const (
	// MinTokenLength keeps minted tokens well above the redactor's minimum
	// so every token is reliably masked.
	MinTokenLength = 16
	// MaxTokenBytes is takod's per-field registry auth cap.
	MaxTokenBytes = 4096

	// ECRRefreshAge re-mints ECR tokens (valid 12h) after 6h.
	ECRRefreshAge = 6 * time.Hour
	// GCRRefreshAge re-mints gcloud access tokens (valid ~60m) after 45m.
	GCRRefreshAge = 45 * time.Minute

	ecrUsername = "AWS"
	gcrUsername = "oauth2accesstoken"

	awsInstall    = "https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html"
	gcloudInstall = "https://cloud.google.com/sdk/docs/install"
	ghInstall     = "https://cli.github.com/"
)

// Credential is a minted registry login.
type Credential struct {
	Username string
	Password string
	// FromGHCLI reports a ghcr token obtained from `gh auth token`, whose
	// default OAuth scopes may lack read:packages.
	FromGHCLI bool
}

// MintError reports a provider credential that could not be minted. It is an
// operation failure (exit 1): the config is valid, and the remedy is a CLI
// install or login on the deploying machine. Its message never contains the
// CLI's stdout.
type MintError struct {
	Host     string
	Provider string
	Err      error
}

func (e *MintError) Error() string {
	return fmt.Sprintf("registry %s: provider %s could not mint a credential: %v", e.Host, e.Provider, e.Err)
}

func (e *MintError) Unwrap() error { return e.Err }

// Options configures a Minter.
type Options struct {
	// Register adds a minted token to the output redactor. It is called
	// before Auth returns the token.
	Register func(string)
	// Redact masks known secrets in provider stderr placed in errors.
	Redact func(string) string
	// Now overrides the clock in tests.
	Now func() time.Time
	// Getenv overrides environment lookups in tests.
	Getenv func(string) string
}

type cachedCredential struct {
	credential Credential
	mintedAt   time.Time
	refreshAge time.Duration
}

// Minter mints and caches provider credentials for one operation.
type Minter struct {
	opts  Options
	mu    sync.Mutex
	cache map[string]cachedCredential
	group singleflight.Group
}

// NewMinter creates a Minter.
func NewMinter(opts Options) *Minter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	return &Minter{opts: opts, cache: map[string]cachedCredential{}}
}

// Auth returns a credential for a provider registry entry, minting it when
// it is not cached or is older than the provider's refresh age. Concurrent
// callers for the same entry share one mint.
func (m *Minter) Auth(ctx context.Context, host string, entry config.RegistryConfig) (Credential, error) {
	provider := config.NormalizeRegistryProvider(entry.Provider)
	if provider == "" {
		return Credential{}, &MintError{Host: host, Provider: entry.Provider, Err: fmt.Errorf("unsupported provider")}
	}
	key := cacheKey(host, provider, entry)
	if credential, ok := m.cached(key); ok {
		return credential, nil
	}
	value, err, _ := m.group.Do(key, func() (any, error) {
		if credential, ok := m.cached(key); ok {
			return credential, nil
		}
		credential, refreshAge, err := m.mint(ctx, host, provider, entry)
		if err != nil {
			return Credential{}, &MintError{Host: host, Provider: entry.Provider, Err: err}
		}
		if m.opts.Register != nil {
			m.opts.Register(credential.Password)
		}
		m.mu.Lock()
		m.cache[key] = cachedCredential{credential: credential, mintedAt: m.opts.Now(), refreshAge: refreshAge}
		m.mu.Unlock()
		return credential, nil
	})
	if err != nil {
		return Credential{}, err
	}
	return value.(Credential), nil
}

func (m *Minter) cached(key string) (Credential, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.cache[key]
	if !ok {
		return Credential{}, false
	}
	if entry.refreshAge > 0 && m.opts.Now().Sub(entry.mintedAt) >= entry.refreshAge {
		delete(m.cache, key)
		return Credential{}, false
	}
	return entry.credential, true
}

func cacheKey(host, provider string, entry config.RegistryConfig) string {
	return strings.Join([]string{host, provider, entry.Region, entry.Profile, entry.Account, entry.Username}, "\x00")
}

func (m *Minter) mint(ctx context.Context, host, provider string, entry config.RegistryConfig) (Credential, time.Duration, error) {
	switch provider {
	case config.RegistryProviderECR:
		credential, err := m.mintECR(ctx, host, entry)
		return credential, ECRRefreshAge, err
	case config.RegistryProviderGCR:
		credential, err := m.mintGCR(ctx, entry)
		return credential, GCRRefreshAge, err
	case config.RegistryProviderGHCR:
		credential, err := m.mintGHCR(ctx, entry)
		return credential, 0, err
	default:
		return Credential{}, 0, fmt.Errorf("unsupported provider")
	}
}

// ECRCommand returns the argv that mints an ECR password for host.
func ECRCommand(host string, entry config.RegistryConfig) ([]string, error) {
	region := strings.TrimSpace(entry.Region)
	if region == "" {
		parsed, ok := config.ECRRegionFromHost(host)
		if !ok {
			return nil, fmt.Errorf("%s is not an ECR registry host", host)
		}
		region = parsed
	}
	args := []string{"ecr", "get-login-password", "--region", region}
	if profile := strings.TrimSpace(entry.Profile); profile != "" {
		args = append(args, "--profile", profile)
	}
	return args, nil
}

// GCRCommand returns the argv that mints a gcloud access token.
func GCRCommand(entry config.RegistryConfig) []string {
	args := []string{"auth", "print-access-token"}
	if account := strings.TrimSpace(entry.Account); account != "" {
		args = append(args, "--account", account)
	}
	return args
}

func (m *Minter) mintECR(ctx context.Context, host string, entry config.RegistryConfig) (Credential, error) {
	args, err := ECRCommand(host, entry)
	if err != nil {
		return Credential{}, err
	}
	token, err := m.run(ctx, "ecr", "aws", awsInstall, "run `aws sso login` or configure AWS credentials", args...)
	if err != nil {
		return Credential{}, err
	}
	if err := ValidateToken(token); err != nil {
		return Credential{}, fmt.Errorf("aws ecr get-login-password returned %v", err)
	}
	return Credential{Username: ecrUsername, Password: token}, nil
}

func (m *Minter) mintGCR(ctx context.Context, entry config.RegistryConfig) (Credential, error) {
	token, err := m.run(ctx, entry.Provider, "gcloud", gcloudInstall, "run `gcloud auth login`", GCRCommand(entry)...)
	if err != nil {
		return Credential{}, err
	}
	if err := ValidateToken(token); err != nil {
		return Credential{}, fmt.Errorf("gcloud auth print-access-token returned %v", err)
	}
	return Credential{Username: gcrUsername, Password: token}, nil
}

func (m *Minter) mintGHCR(ctx context.Context, entry config.RegistryConfig) (Credential, error) {
	var credential Credential
	source := ""
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if value := strings.TrimSpace(m.opts.Getenv(name)); value != "" {
			credential.Password = value
			source = name
			break
		}
	}
	if source == "" {
		token, err := m.run(ctx, "ghcr", "gh", ghInstall, "run `gh auth login`", "auth", "token")
		if err != nil {
			return Credential{}, err
		}
		credential.Password = token
		credential.FromGHCLI = true
		source = "gh auth token"
	}
	if err := ValidateToken(credential.Password); err != nil {
		return Credential{}, fmt.Errorf("%s returned %v", source, err)
	}

	username := strings.TrimSpace(entry.Username)
	if username == "" {
		username = strings.TrimSpace(m.opts.Getenv("GITHUB_ACTOR"))
	}
	if username == "" {
		login, err := m.run(ctx, "ghcr", "gh", ghInstall, "set registries.ghcr.io.username or GITHUB_ACTOR, or run `gh auth login`", "api", "user", "--jq", ".login")
		if err != nil {
			return Credential{}, fmt.Errorf("resolve GitHub username: %w", err)
		}
		username = strings.TrimSpace(login)
	}
	if username == "" || hasControlChars(username) || len(username) > MaxTokenBytes {
		return Credential{}, fmt.Errorf("could not resolve a valid GitHub username; set registries.ghcr.io.username or GITHUB_ACTOR")
	}
	credential.Username = username
	return credential, nil
}

// run executes a provider CLI with stdout kept out of every error.
func (m *Minter) run(ctx context.Context, provider, command, install, loginHint string, args ...string) (string, error) {
	if _, err := providercli.LookPath(command); err != nil {
		return "", providercli.MissingCLIError(provider, command, install)
	}
	output, err := providercli.Run(ctx, providercli.Options{
		ExcludeStdoutFromErrors: true,
		RedactStderr:            m.opts.Redact,
	}, command, args...)
	if err != nil {
		return "", fmt.Errorf("%w (%s)", err, loginHint)
	}
	return strings.TrimSpace(output), nil
}

// ValidateToken rejects empty, short, oversized or control-character
// tokens. The returned error never contains the value.
func ValidateToken(token string) error {
	switch {
	case token == "":
		return fmt.Errorf("an empty token")
	case hasControlChars(token):
		return fmt.Errorf("a malformed token (control characters)")
	case len(token) < MinTokenLength:
		return fmt.Errorf("a malformed token (shorter than %d characters)", MinTokenLength)
	case len(token) > MaxTokenBytes:
		return fmt.Errorf("a token longer than %d bytes, which takod rejects", MaxTokenBytes)
	}
	return nil
}

func hasControlChars(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
