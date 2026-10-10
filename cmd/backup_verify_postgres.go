package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

var postgresVerifyOptions struct {
	Server        string
	Service       string
	ArtifactID    string
	ObjectKey     string
	Size          int64
	SHA256        string
	Image         string
	Database      string
	Username      string
	Query         string
	ExpectedValue string
	ScratchBytes  int64
}

var acquirePostgresVerificationLeasesWith = engine.AcquireRemoteOperationLeasesContext
var requirePostgresVerificationCapabilityWith = takodclient.RequireCapability
var newPostgresVerificationClientFactoryWith = nodeclient.NewFactory

var backupVerifyPostgresCmd = &cobra.Command{
	Use:          "verify-postgres",
	Short:        "Verify a remote pg_dump artifact in an isolated PostgreSQL instance",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	RunE:         runBackupVerifyPostgres,
}

func init() {
	backupCmd.AddCommand(backupVerifyPostgresCmd)
	flags := backupVerifyPostgresCmd.Flags()
	flags.StringVarP(&postgresVerifyOptions.Server, "server", "s", "", "Node that will run the isolated verification")
	flags.StringVar(&postgresVerifyOptions.Service, "service", "", "Service whose backup storage configuration should be used")
	flags.StringVar(&postgresVerifyOptions.ArtifactID, "artifact-id", "", "Immutable artifact identifier")
	flags.StringVar(&postgresVerifyOptions.ObjectKey, "object-key", "", "Exact S3-compatible object key")
	flags.Int64Var(&postgresVerifyOptions.Size, "size", 0, "Exact artifact size in bytes")
	flags.StringVar(&postgresVerifyOptions.SHA256, "sha256", "", "Expected lowercase SHA-256 digest")
	flags.StringVar(&postgresVerifyOptions.Image, "postgres-image", "", "Digest-pinned PostgreSQL image used for isolated restore")
	flags.StringVar(&postgresVerifyOptions.Database, "database", "postgres", "Scratch database name")
	flags.StringVar(&postgresVerifyOptions.Username, "username", "postgres", "Scratch database username")
	flags.StringVar(&postgresVerifyOptions.Query, "query", "", "Read-only scalar verification query")
	flags.StringVar(&postgresVerifyOptions.ExpectedValue, "expected", "", "Exact trimmed scalar query result")
	flags.Int64Var(&postgresVerifyOptions.ScratchBytes, "scratch-size", 0, "Reserved scratch storage budget in bytes")
}

type postgresVerificationCommandResult struct {
	APIVersion   string                           `json:"apiVersion"`
	Kind         string                           `json:"kind"`
	Project      string                           `json:"project"`
	Environment  string                           `json:"environment"`
	Server       string                           `json:"server"`
	Verification takod.PostgresVerificationResult `json:"verification"`
}

func runBackupVerifyPostgres(cmd *cobra.Command, _ []string) error {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return formatConfigLoadError(cfgFile, err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return err
	}
	envName := getEnvironmentName(cfg)
	environment, ok := cfg.Environments[envName]
	if !ok {
		return fmt.Errorf("environment %s not found", envName)
	}
	serviceName := strings.TrimSpace(postgresVerifyOptions.Service)
	service, ok := environment.Services[serviceName]
	if !ok {
		return fmt.Errorf("service %s not found in environment %s", serviceName, envName)
	}
	if service.Backup == nil || service.Backup.Storage == nil {
		return fmt.Errorf("service %s must configure backup.storage for remote verification", serviceName)
	}
	storage := takodBackupStorageFromConfig(service.Backup.Storage)
	if storage == nil {
		return fmt.Errorf("service %s backup storage is unavailable", serviceName)
	}
	request := takod.PostgresVerificationRequest{
		Project: cfg.Project.Name, Environment: envName, Service: serviceName, Storage: *storage,
		Artifact: takod.BackupArtifactReference{
			ID: strings.TrimSpace(postgresVerifyOptions.ArtifactID), Format: takod.BackupArtifactFormatPostgresCustom,
			Key: strings.TrimSpace(postgresVerifyOptions.ObjectKey), Size: postgresVerifyOptions.Size,
			SHA256: strings.TrimSpace(postgresVerifyOptions.SHA256),
		},
		Image: strings.TrimSpace(postgresVerifyOptions.Image), Database: strings.TrimSpace(postgresVerifyOptions.Database),
		Username: strings.TrimSpace(postgresVerifyOptions.Username), Query: postgresVerifyOptions.Query,
		ExpectedValue: postgresVerifyOptions.ExpectedValue, ScratchBytes: postgresVerifyOptions.ScratchBytes,
	}
	if err := takod.ValidatePostgresVerificationRequest(request); err != nil {
		return &engine.InvalidRequestError{Err: err}
	}

	servers, err := resolveEnvironmentServerSet(cfg, envName, postgresVerifyOptions.Server)
	if err != nil {
		return err
	}
	servers, targetNames, err := schedulableMutationServerSet(cfg, envName, servers, true)
	if err != nil {
		return err
	}
	if err := ensureSingleBackupRestoreTarget(targetNames, postgresVerifyOptions.Server); err != nil {
		return fmt.Errorf("PostgreSQL verification requires one node: %w", err)
	}
	target := targetNames[0]
	if _, ok := servers[target]; !ok {
		return fmt.Errorf("verification target %s is unavailable", target)
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	pool := ssh.NewPool()
	defer pool.CloseAll()
	factory, err := newPostgresVerificationClientFactoryWith(cfg, pool, takodSocketFromConfig(cfg))
	if err != nil {
		return err
	}
	defer factory.CloseIdleConnections()
	client, _, err := factory.Client(ctx, target)
	if err != nil {
		return fmt.Errorf("connect to verification node %s: %w", target, err)
	}
	if err := requirePostgresVerificationCapabilityWith(ctx, client, takodSocketFromConfig(cfg), target, takod.CapabilityBackupPostgresVerifyV1, "isolated PostgreSQL backup verification"); err != nil {
		return err
	}
	leaseSet, err := acquirePostgresVerificationLeasesWith(ctx, pool, cfg, envName, targetNames, "backup-postgres-verify")
	if err != nil {
		return err
	}
	defer leaseSet.Release()
	leaseCtx, cancel := leaseSet.BindContext(ctx)
	defer cancel()
	ctx = leaseCtx
	factory.SetOperationFenceSource(leaseSet)

	var verification takod.PostgresVerificationResult
	output, err := client.RequestJSONWithTimeout(ctx, "POST", "/v1/backups/postgres/verify", request, 6*time.Hour)
	if err != nil {
		var httpErr *takodclient.HTTPError
		if errors.As(err, &httpErr) && decodeTakodJSON(httpErr.Body, &verification) == nil && verification.ArtifactID != "" {
			result := newPostgresVerificationCommandResult(cfg, envName, target, verification)
			if machineOutputEnabled() {
				if emitErr := emitResultDocument(result); emitErr != nil {
					return emitErr
				}
			} else {
				fmt.Fprintf(humanOut(), "PostgreSQL backup artifact %s failed verification at %s; cleanup complete: %t\n", verification.ArtifactID, verification.FailureStage, verification.CleanupComplete)
			}
		}
		return fmt.Errorf("PostgreSQL backup verification failed: %w", err)
	}
	if err := decodeTakodJSON(output, &verification); err != nil {
		return fmt.Errorf("decode PostgreSQL backup verification result: %w", err)
	}
	result := newPostgresVerificationCommandResult(cfg, envName, target, verification)
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	fmt.Fprintf(humanOut(), "PostgreSQL backup artifact %s verified on %s\n", verification.ArtifactID, target)
	fmt.Fprintf(humanOut(), "SHA-256: %s\n", verification.SHA256)
	fmt.Fprintln(humanOut(), "Scratch container, data workspace, and downloaded artifact were removed.")
	return nil
}

func newPostgresVerificationCommandResult(cfg *config.Config, environment, server string, verification takod.PostgresVerificationResult) postgresVerificationCommandResult {
	return postgresVerificationCommandResult{
		APIVersion: takoapi.APIVersionCurrent, Kind: "PostgresBackupVerificationResult",
		Project: cfg.Project.Name, Environment: environment, Server: server, Verification: verification,
	}
}
