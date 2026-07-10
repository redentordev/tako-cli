package deployer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	remotestate "github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
)

func TestPrepareServiceFilesPreservesBinaryDirectoryAndSecretModes(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "relay")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	binary := []byte{0x00, 0xff, 0x01, 0x02}
	if err := os.WriteFile(filepath.Join(configDir, "credentials.json"), binary, 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Project: config.ProjectConfig{Name: "demo"}}
	d := &Deployer{config: cfg, environment: "production"}
	service := &config.ServiceConfig{Files: []config.ServiceFileConfig{{Source: configDir, Target: "/work/.relay", Secret: true, Owner: "1000:1001"}}}
	bundles, mounts, hash, err := d.PrepareServiceFiles("relay", service)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 1 || !bundles[0].Directory || len(mounts) != 1 || !strings.HasSuffix(mounts[0], ",readonly") {
		t.Fatalf("payload/mounts = %#v %#v", bundles, mounts)
	}
	if bundles[0].UID != 1000 || bundles[0].GID != 1001 {
		t.Fatalf("bundle ownership = %d:%d", bundles[0].UID, bundles[0].GID)
	}
	var found bool
	for _, entry := range bundles[0].Entries {
		if entry.Path == "credentials.json" {
			found = true
			if !bytes.Equal(entry.Data, binary) || entry.Mode != 0600 {
				t.Fatalf("binary secret entry = %#v", entry)
			}
		}
	}
	if !found || !strings.HasPrefix(hash, "sha256:") {
		t.Fatalf("found/hash = %v %q", found, hash)
	}
	repeated, _, repeatedHash, err := d.PrepareServiceFiles("relay", service)
	if err != nil || repeatedHash != hash || len(repeated) != len(bundles) {
		t.Fatalf("repeated payload/hash = %#v %q %v", repeated, repeatedHash, err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "credentials.json"), []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	_, _, changedHash, err := d.PrepareServiceFiles("relay", service)
	if err != nil || changedHash == hash {
		t.Fatalf("changed hash = %q, err=%v", changedHash, err)
	}
	service.FilesContentHash = hash
	if _, _, _, err := d.PrepareServiceFiles("relay", service); err == nil || !strings.Contains(err.Error(), "changed after planning") {
		t.Fatalf("plan drift error = %v", err)
	}
}

func TestApplyRollbackServiceFilesUsesHistoricalMetadata(t *testing.T) {
	service := &config.ServiceConfig{Files: []config.ServiceFileConfig{{Source: "today", Target: "/today"}}}
	history := &remotestate.ServiceState{
		FilesContentHash: "sha256:" + strings.Repeat("b", 64),
		Files: []remotestate.ServiceFileState{
			{Target: "/historical/first", Secret: true, Owner: "1000:1001"},
			{Target: "/historical/second"},
		},
	}
	if err := applyRollbackServiceFiles(service, history); err != nil {
		t.Fatal(err)
	}
	if !service.ReuseFiles || service.FilesContentHash != history.FilesContentHash || len(service.Files) != 2 {
		t.Fatalf("rollback files = %#v", service)
	}
	if service.Files[0].Source != "" || service.Files[0].Target != "/historical/first" || !service.Files[0].Secret || service.Files[0].Owner != "1000:1001" {
		t.Fatalf("historical first file = %#v", service.Files[0])
	}
}

func TestApplyRollbackServiceFilesClearsFilesForHistoricalRevisionWithoutThem(t *testing.T) {
	service := &config.ServiceConfig{
		Files:            []config.ServiceFileConfig{{Source: "today", Target: "/today"}},
		FilesContentHash: "sha256:" + strings.Repeat("a", 64),
		ReuseFiles:       true,
	}
	if err := applyRollbackServiceFiles(service, &remotestate.ServiceState{}); err != nil {
		t.Fatal(err)
	}
	if len(service.Files) != 0 || service.FilesContentHash != "" || service.ReuseFiles {
		t.Fatalf("rollback files were not cleared: %#v", service)
	}
}

func TestApplyRollbackServiceFilesRejectsHashWithoutMountMetadata(t *testing.T) {
	service := &config.ServiceConfig{}
	err := applyRollbackServiceFiles(service, &remotestate.ServiceState{FilesContentHash: "sha256:" + strings.Repeat("a", 64)})
	if err == nil || !strings.Contains(err.Error(), "mount metadata") {
		t.Fatalf("error = %v", err)
	}
}

func TestPrepareServiceFilesReuseDoesNotReadCurrentSource(t *testing.T) {
	hash := "sha256:" + strings.Repeat("a", 64)
	service := &config.ServiceConfig{
		Files:            []config.ServiceFileConfig{{Source: "/missing/current/config", Target: "/etc/demo/config"}},
		FilesContentHash: hash, ReuseFiles: true,
	}
	bundles, mounts, gotHash, err := PrepareServiceFilesPayload("demo", "production", "web", service)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 0 || len(mounts) != 1 || gotHash != hash || !strings.Contains(mounts[0], strings.Repeat("a", 64)) {
		t.Fatalf("reuse payload = bundles=%#v mounts=%#v hash=%q", bundles, mounts, gotHash)
	}
}
