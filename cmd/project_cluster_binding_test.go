package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/projectbinding"
	"github.com/spf13/cobra"
)

func deployPlatformConfig() *config.Config {
	return &config.Config{
		Project: config.ProjectConfig{Name: "demo", Version: "1.0.0"},
		Platform: &config.PlatformContext{
			ClusterID:   "11111111-1111-4111-8111-111111111111",
			LocalNodeID: "22222222-2222-4222-8222-222222222222", LocalNodeName: "node-2",
			ControllerNodeID: "33333333-3333-4333-8333-333333333333", ControllerNodeName: "node-1",
			InventoryGeneration: 9, InventoryUpdatedAt: time.Date(2026, 7, 17, 10, 0, 0, 0, time.UTC),
		},
	}
}

func TestEnsureDeployClusterAttachmentRequiresExactAcknowledgement(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "1")
	configPath := filepath.Join(t.TempDir(), "tako.yaml")
	_, err := prepareDeployClusterAttachment(&cobra.Command{}, deployPlatformConfig(), configPath, "")
	var invalid *engine.InvalidRequestError
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "--accept-cluster 11111111-1111-4111-8111-111111111111") || !strings.Contains(err.Error(), "node-2") || !strings.Contains(err.Error(), "node-1") {
		t.Fatalf("attachment error = %T %v", err, err)
	}
	path, _ := projectbinding.PathForConfig(configPath)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("binding unexpectedly created: %v", statErr)
	}
}

func TestPrepareDeployClusterAttachmentDefersPersistenceUntilApply(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "tako.yaml")
	cmd := &cobra.Command{}
	var output bytes.Buffer
	cmd.SetOut(&output)
	cfg := deployPlatformConfig()
	pending, err := prepareDeployClusterAttachment(cmd, cfg, configPath, cfg.Platform.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	path, _ := projectbinding.PathForConfig(configPath)
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("plan preparation persisted binding: %v", statErr)
	}
	if err := commitDeployClusterAttachment(cmd, pending); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Attached project demo") {
		t.Fatalf("output = %q", output.String())
	}
	reused, err := prepareDeployClusterAttachment(cmd, cfg, configPath, "")
	if err != nil || reused != nil {
		t.Fatalf("reuse binding = %#v, err = %v", reused, err)
	}
	binding, err := projectbinding.ReadOptional(path)
	if err != nil || binding == nil || binding.Project != "demo" {
		t.Fatalf("binding = %#v, err = %v", binding, err)
	}
}

func TestEnsureDeployClusterAttachmentRejectsMismatchAndUnexpectedFlag(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "tako.yaml")
	cfg := deployPlatformConfig()
	_, err := prepareDeployClusterAttachment(&cobra.Command{}, cfg, configPath, "44444444-4444-4444-8444-444444444444")
	if err == nil || !strings.Contains(err.Error(), "does not match detected") {
		t.Fatalf("cluster mismatch = %v", err)
	}
	if _, err := prepareDeployClusterAttachment(&cobra.Command{}, &config.Config{}, configPath, cfg.Platform.ClusterID); err == nil || !strings.Contains(err.Error(), "no platform cluster") {
		t.Fatalf("unexpected flag = %v", err)
	}
}

func TestProjectMutationPreflightRejectsEveryUnattachedMutationCategory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "tako.yaml")
	cfg := deployPlatformConfig()
	for _, command := range []*cobra.Command{destroyCmd, scaleCmd, rollbackCmd, removeCmd, placementApplyCmd, execCmd, jobsTriggerCmd, upgradeServersCmd, liveCmd} {
		t.Run(command.Name(), func(t *testing.T) {
			if command.Annotations[projectMutationAnnotation] != "true" {
				t.Fatalf("%s lacks centralized mutation annotation", command.CommandPath())
			}
			err := requireProjectMutationAttachmentForConfig(command, cfg, configPath)
			var invalid *engine.InvalidRequestError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "tako project attach --cluster") {
				t.Fatalf("%s bypassed attachment: %T %v", command.CommandPath(), err, err)
			}
		})
	}
}

func TestProjectMutationPreflightRequiresOffNodeAttachmentAndRejectsIncompleteLocalState(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "tako.yaml")
	offNode := &config.Config{
		Project: config.ProjectConfig{Name: "demo"},
		Servers: map[string]config.ServerConfig{
			"node-1": {ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222"},
			"node-2": {ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "33333333-3333-4333-8333-333333333333"},
		},
	}
	if err := requireProjectMutationAttachmentForConfig(destroyCmd, offNode, configPath); err == nil || !strings.Contains(err.Error(), "project attach --cluster") {
		t.Fatalf("off-node missing binding bypass = %v", err)
	}
	pending, err := prepareDeployClusterAttachment(&cobra.Command{}, offNode, configPath, "11111111-1111-4111-8111-111111111111")
	if err != nil || pending == nil || pending.binding.LocalNodeID != "" {
		t.Fatalf("off-node explicit attachment = %#v, err = %v", pending, err)
	}

	incomplete := &config.Config{Project: config.ProjectConfig{Name: "demo"}, PlatformArtifacts: []string{"/etc/tako/identity.json"}}
	if err := requireProjectMutationAttachmentForConfig(destroyCmd, incomplete, configPath); err == nil || !strings.Contains(err.Error(), "platform inspect") {
		t.Fatalf("incomplete local platform bypass = %v", err)
	}
	if _, err := prepareDeployClusterAttachment(&cobra.Command{}, incomplete, configPath, ""); err == nil || !strings.Contains(err.Error(), "platform inspect") {
		t.Fatalf("deploy incomplete platform bypass = %v", err)
	}
}

func TestConfiglessRunRefusesLocalPlatformArtifacts(t *testing.T) {
	root := t.TempDir()
	for _, artifact := range []string{"identity", "binding", "inventory", "config", "membership", "worker-unit", "mesh-key"} {
		t.Run(artifact, func(t *testing.T) {
			paths := make([]string, 7)
			for index := range paths {
				paths[index] = filepath.Join(root, artifact, fmt.Sprintf("missing-%d", index))
			}
			if err := os.MkdirAll(filepath.Dir(paths[0]), 0700); err != nil {
				t.Fatal(err)
			}
			paths[map[string]int{"identity": 0, "binding": 1, "inventory": 2, "config": 3, "membership": 4, "worker-unit": 5, "mesh-key": 6}[artifact]] = filepath.Join(root, artifact, "present")
			if err := os.WriteFile(paths[map[string]int{"identity": 0, "binding": 1, "inventory": 2, "config": 3, "membership": 4, "worker-unit": 5, "mesh-key": 6}[artifact]], []byte("trusted marker"), 0600); err != nil {
				t.Fatal(err)
			}
			err := rejectConfiglessRunOnEnrolledPlatform(paths)
			var invalid *engine.InvalidRequestError
			if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "project attach") {
				t.Fatalf("configless platform guard = %T %v", err, err)
			}
		})
	}
	if err := rejectConfiglessRunOnEnrolledPlatform([]string{filepath.Join(root, "missing"), filepath.Join(root, "also-missing")}); err != nil {
		t.Fatalf("unenrolled configless run rejected: %v", err)
	}
}

func TestMissingAttachmentEmitsStructuredMachineResult(t *testing.T) {
	t.Setenv("TAKO_NONINTERACTIVE", "1")
	configPath := filepath.Join(t.TempDir(), "tako.yaml")
	for _, mode := range []struct {
		name, output, events string
	}{
		{name: "json", output: outputFormatJSON},
		{name: "ndjson", output: outputFormatText, events: eventsFormatNDJSON},
	} {
		t.Run(mode.name, func(t *testing.T) {
			withMachineOutput(t, mode.output, mode.events, func() {
				var prepareErr error
				stdout := captureStdout(t, func() {
					_, prepareErr = prepareDeployClusterAttachment(&cobra.Command{}, deployPlatformConfig(), configPath, "")
				})
				var invalid *engine.InvalidRequestError
				if !errors.As(prepareErr, &invalid) {
					t.Fatalf("error = %T %v", prepareErr, prepareErr)
				}
				if mode.events == "" {
					var document clusterAttachmentRequiredDocument
					if err := json.Unmarshal([]byte(stdout), &document); err != nil {
						t.Fatalf("decode JSON %q: %v", stdout, err)
					}
					if document.Kind != "ClusterAttachmentRequired" || document.ClusterID == "" || document.Acknowledgement == "" {
						t.Fatalf("document = %#v", document)
					}
				} else if !strings.Contains(stdout, `"type":"result"`) || !strings.Contains(stdout, `"kind":"ClusterAttachmentRequired"`) {
					t.Fatalf("NDJSON = %q", stdout)
				}
			})
		})
	}
}
