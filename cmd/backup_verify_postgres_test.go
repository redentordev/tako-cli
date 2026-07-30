package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/nodeclient"
	"github.com/redentordev/tako-cli/pkg/nodeidentity"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
	"github.com/spf13/cobra"
)

func TestRunBackupVerifyPostgresMachineContract(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("local transport identity test requires a non-root peer UID")
	}
	dir, err := os.MkdirTemp("/tmp", "tako-pgv-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "takod.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var postCount int
	var runtimeIdentity nodeidentity.Identity
	responseStatus := http.StatusOK
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status" {
			_ = json.NewEncoder(w).Encode(takodclient.AgentStatus{Runtime: "takod", Capabilities: []string{nodeidentity.Capability}, Identity: &runtimeIdentity})
			return
		}
		if r.URL.Path != "/v1/backups/postgres/verify" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		postCount++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(responseStatus)
		_ = json.NewEncoder(w).Encode(takod.PostgresVerificationResult{
			SchemaVersion: 1, Project: "demo", Environment: "production", Service: "db", ArtifactID: "dump-1",
			ArtifactFormat: takod.BackupArtifactFormatPostgresCustom, ObjectKey: "dumps/dump-1.dump", StorageProvider: "s3", StorageBucket: "backups",
			Size: 4, ScratchBytes: 64 << 20, MemoryBytes: 1 << 30, SHA256: strings.Repeat("a", 64), Status: map[bool]string{true: "verified", false: "failed"}[responseStatus == http.StatusOK],
			FailureStage: map[bool]string{true: "", false: "restore"}[responseStatus == http.StatusOK], CleanupComplete: true,
		})
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	identity, err := nodeidentity.New("", "", "node-a", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runtimeIdentity = identity.Identity
	configPath := filepath.Join(dir, "tako.yaml")
	configBody := fmt.Sprintf(`project:
  name: demo
  version: 1.0.0
runtime:
  mode: takod
  proxy: tako-proxy
  agent:
    socket: %s
mesh:
  enabled: true
state:
  backend: replicated
  deployConsistency: lease
  onUnreachableNode: block
  remoteCacheEnabled: true
servers:
  node-a:
    host: localhost
    user: root
    transport: local
    clusterId: %s
    nodeId: %s
    workerUid: %d
environments:
  production:
    servers: [node-a]
    services:
      db:
        image: postgres:16
        backup:
          storage:
            provider: s3
            bucket: backups
            region: us-east-1
            accessKeyId: access
            secretAccessKey: secret
`, socket, identity.ClusterID, identity.NodeID, os.Geteuid())
	if err := os.WriteFile(configPath, []byte(configBody), 0600); err != nil {
		t.Fatal(err)
	}

	oldCfg, oldEnv, oldOutput, oldEvents := cfgFile, envFlag, outputFormatFlag, eventsFormatFlag
	oldOptions := postgresVerifyOptions
	oldAcquire, oldRequire, oldFactory := acquirePostgresVerificationLeasesWith, requirePostgresVerificationCapabilityWith, newPostgresVerificationClientFactoryWith
	t.Cleanup(func() {
		cfgFile, envFlag, outputFormatFlag, eventsFormatFlag = oldCfg, oldEnv, oldOutput, oldEvents
		postgresVerifyOptions = oldOptions
		acquirePostgresVerificationLeasesWith, requirePostgresVerificationCapabilityWith = oldAcquire, oldRequire
		newPostgresVerificationClientFactoryWith = oldFactory
	})
	cfgFile, envFlag = configPath, "production"
	postgresVerifyOptions.Server, postgresVerifyOptions.Service = "node-a", "db"
	postgresVerifyOptions.ArtifactID, postgresVerifyOptions.ObjectKey = "dump-1", "dumps/dump-1.dump"
	postgresVerifyOptions.Size, postgresVerifyOptions.ScratchBytes = 4, 64<<20
	postgresVerifyOptions.SHA256 = strings.Repeat("a", 64)
	postgresVerifyOptions.Image = "postgres:16-alpine@sha256:" + strings.Repeat("0", 64)
	postgresVerifyOptions.Database, postgresVerifyOptions.Username = "app", "postgres"
	postgresVerifyOptions.Query, postgresVerifyOptions.ExpectedValue = "SELECT 1", "1"
	var requiredCapability, leasedOperation string
	requirePostgresVerificationCapabilityWith = func(_ context.Context, _ any, _, _, capability, _ string) error {
		requiredCapability = capability
		return nil
	}
	acquirePostgresVerificationLeasesWith = func(_ context.Context, _ *ssh.Pool, _ *config.Config, _ string, _ []string, operation string) (*engine.RemoteLeaseSet, error) {
		leasedOperation = operation
		return engine.NewRemoteLeaseSet(operation, nil), nil
	}
	newPostgresVerificationClientFactoryWith = func(cfg *config.Config, pool nodeclient.SSHPool, agentSocket string) (*nodeclient.Factory, error) {
		return nodeclient.NewFactoryWithLocalSocket(cfg, pool, agentSocket, socket)
	}

	outputFormatFlag, eventsFormatFlag = outputFormatJSON, ""
	var runErr error
	stdout := captureStdout(t, func() { runErr = runBackupVerifyPostgres(&cobra.Command{}, nil) })
	if runErr != nil || !strings.Contains(stdout, `"kind": "PostgresBackupVerificationResult"`) || strings.Contains(stdout, "access") || strings.Contains(stdout, "secret") || postCount != 1 || requiredCapability != takod.CapabilityBackupPostgresVerifyV1 || leasedOperation != "backup-postgres-verify" {
		t.Fatalf("JSON success = %q, err=%v, posts=%d", stdout, runErr, postCount)
	}

	responseStatus = http.StatusUnprocessableEntity
	stdout = captureStdout(t, func() { runErr = runBackupVerifyPostgres(&cobra.Command{}, nil) })
	if runErr == nil || !strings.Contains(stdout, `"failureStage": "restore"`) || postCount != 2 {
		t.Fatalf("JSON failure = %q, err=%v, posts=%d", stdout, runErr, postCount)
	}

	responseStatus = http.StatusOK
	outputFormatFlag, eventsFormatFlag = "", eventsFormatNDJSON
	stdout = captureStdout(t, func() { runErr = runBackupVerifyPostgres(&cobra.Command{}, nil) })
	if runErr != nil || !strings.Contains(stdout, `"type":"result"`) || postCount != 3 {
		t.Fatalf("NDJSON success = %q, err=%v, posts=%d", stdout, runErr, postCount)
	}

	requirePostgresVerificationCapabilityWith = func(context.Context, any, string, string, string, string) error {
		return fmt.Errorf("capability missing")
	}
	if err := runBackupVerifyPostgres(&cobra.Command{}, nil); err == nil || !strings.Contains(err.Error(), "capability missing") || postCount != 3 {
		t.Fatalf("capability rejection err=%v posts=%d", err, postCount)
	}
	requirePostgresVerificationCapabilityWith = func(context.Context, any, string, string, string, string) error { return nil }
	acquirePostgresVerificationLeasesWith = func(context.Context, *ssh.Pool, *config.Config, string, []string, string) (*engine.RemoteLeaseSet, error) {
		return nil, fmt.Errorf("fence lease failed")
	}
	if err := runBackupVerifyPostgres(&cobra.Command{}, nil); err == nil || !strings.Contains(err.Error(), "fence lease failed") || postCount != 3 {
		t.Fatalf("lease/fence rejection err=%v posts=%d", err, postCount)
	}
}
