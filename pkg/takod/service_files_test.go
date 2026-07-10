package takod

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestPrepareServiceFilesAtomicallyPublishesBinaryTreeAndCleansStale(t *testing.T) {
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	bundle := ServiceFileBundle{
		Name: "file-000", Target: "/etc/relay", Directory: true, Secret: true,
		UID: os.Getuid(), GID: os.Getgid(),
		Entries: []ServiceFileEntry{
			{Path: "", Mode: 0700, Directory: true},
			{Path: "nested", Mode: 0700, Directory: true},
			{Path: "nested/credentials.json", Data: []byte{0x00, 0xff, 0x01}, Mode: 0600},
		},
	}
	setID := testServiceFileSetID(t, []ServiceFileBundle{bundle})
	if err := prepareServiceFiles(context.Background(), "demo", "production", "relay", setID, []ServiceFileBundle{bundle}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(serviceFilesRoot, "demo", "production", "relay", setID, "file-000", "nested", "credentials.json")
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 3 || data[1] != 0xff {
		t.Fatalf("published data = %#v, err=%v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("published mode = %v, err=%v", info.Mode().Perm(), err)
	}
	bundle.Entries[2].Data = []byte("replacement")
	secondSetID := testServiceFileSetID(t, []ServiceFileBundle{bundle})
	if err := prepareServiceFiles(context.Background(), "demo", "production", "relay", secondSetID, []ServiceFileBundle{bundle}); err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(serviceFilesRoot, "demo", "production", "relay", secondSetID, "file-000", "nested", "credentials.json")
	data, _ = os.ReadFile(secondPath)
	if string(data) != "replacement" {
		t.Fatalf("replacement data = %q", data)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("previous immutable version was removed: %v", err)
	}
	if err := removeServiceFiles("demo", "production", "relay"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(filepath.Dir(path))); !os.IsNotExist(err) {
		t.Fatalf("stale service root still exists: %v", err)
	}
}

func TestValidateServiceFileBundlesRejectsTraversalAndPublicSecretMode(t *testing.T) {
	tests := []ServiceFileBundle{
		{Name: "file-000", Target: "/etc/config", Entries: []ServiceFileEntry{{Path: "../escape", Data: []byte("x"), Mode: 0600}}},
		{Name: "file-000", Target: "/etc/config", Secret: true, Entries: []ServiceFileEntry{{Data: []byte("x"), Mode: 0644}}},
		{Name: "file-000", Target: "/etc//config", Entries: []ServiceFileEntry{{Data: []byte("x"), Mode: 0600}}},
		{Name: "file-000", Target: "/etc/config", Directory: true, Entries: []ServiceFileEntry{{Path: "", Mode: 0700, Directory: true}, {Path: "a", Data: []byte("x"), Mode: 0600}, {Path: "a/b", Data: []byte("x"), Mode: 0600}}},
		{Name: "file-000", Target: "/etc/config", Directory: true, Entries: []ServiceFileEntry{{Path: "", Mode: 0700, Directory: true}, {Path: "a/b", Data: []byte("x"), Mode: 0600}}},
	}
	for i, bundle := range tests {
		if err := validateServiceFileBundles([]ServiceFileBundle{bundle}); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestPrepareServiceFilesRejectsMismatchedContentAddress(t *testing.T) {
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	bundle := ServiceFileBundle{Name: "file-000", Target: "/etc/config", UID: os.Getuid(), GID: os.Getgid(), Entries: []ServiceFileEntry{{Data: []byte("content"), Mode: 0644}}}
	if err := prepareServiceFiles(context.Background(), "demo", "production", "web", strings.Repeat("a", 64), []ServiceFileBundle{bundle}); err == nil {
		t.Fatal("mismatched content address accepted")
	}
}

func TestPrepareServiceFilesAppliesNumericOwnership(t *testing.T) {
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	bundle := ServiceFileBundle{Name: "file-000", Target: "/run/secret", Secret: true, UID: os.Getuid(), GID: os.Getgid(), Entries: []ServiceFileEntry{{Data: []byte("secret"), Mode: 0600}}}
	setID := testServiceFileSetID(t, []ServiceFileBundle{bundle})
	if err := prepareServiceFiles(context.Background(), "demo", "production", "relay", setID, []ServiceFileBundle{bundle}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(serviceFilesRoot, "demo", "production", "relay", setID, "file-000"))
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != os.Getuid() || int(stat.Gid) != os.Getgid() || info.Mode().Perm() != 0600 {
		t.Fatalf("ownership/mode = %d:%d %o", stat.Uid, stat.Gid, info.Mode().Perm())
	}
}

func TestJobApplyPublishesButDoesNotPersistFileContent(t *testing.T) {
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	dataDir := t.TempDir()
	scheduler := NewJobScheduler(dataDir)
	secret := []byte("relay-secret-json")
	files := []ServiceFileBundle{{Name: "file-000", Target: "/run/secret", Secret: true, UID: os.Getuid(), GID: os.Getgid(), Entries: []ServiceFileEntry{{Data: secret, Mode: 0600}}}}
	setID := testServiceFileSetID(t, files)
	if err := PublishServiceFiles(context.Background(), ServiceFilesRequest{Project: "demo", Environment: "production", Service: "bootstrap", FileSetID: setID, Files: files}); err != nil {
		t.Fatal(err)
	}
	request := JobsApplyRequest{Project: "demo", Environment: "production", Jobs: []JobSpec{{
		Name: "bootstrap", Schedule: "@daily", Image: "busybox:1.36", Command: []string{"true"},
		Mounts:    []string{"type=bind,source=/var/lib/tako/files/demo/production/bootstrap/" + setID + "/file-000,target=/run/secret,readonly"},
		FileSetID: setID,
	}}}
	if _, err := scheduler.Apply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(jobSpecPath(dataDir, "demo", "production", "bootstrap"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(persisted, secret) || bytes.Contains(persisted, []byte("cmVsYXktc2VjcmV0LWpzb24=")) || bytes.Contains(persisted, []byte(`"files"`)) {
		t.Fatalf("persisted job spec contains request-scoped file data: %s", persisted)
	}
	published, err := os.ReadFile(filepath.Join(serviceFilesRoot, "demo", "production", "bootstrap", setID, "file-000"))
	if err != nil || !bytes.Equal(published, secret) {
		t.Fatalf("published job file = %q, err=%v", published, err)
	}
}

func TestEnsureServiceFileSetRejectsTamperedContent(t *testing.T) {
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	bundle := ServiceFileBundle{Name: "file-000", Target: "/etc/config", UID: os.Getuid(), GID: os.Getgid(), Entries: []ServiceFileEntry{{Data: []byte("original"), Mode: 0644}}}
	setID := testServiceFileSetID(t, []ServiceFileBundle{bundle})
	if err := prepareServiceFiles(context.Background(), "demo", "production", "web", setID, []ServiceFileBundle{bundle}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(serviceFilesRoot, "demo", "production", "web", setID, "file-000")
	if err := os.WriteFile(path, []byte("tampered"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ensureServiceFileSet("demo", "production", "web", setID); err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("tampered set error = %v", err)
	}
}

func testServiceFileSetID(t *testing.T, files []ServiceFileBundle) string {
	t.Helper()
	data, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
