package nodeidentity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testClusterID = "11111111-1111-4111-8111-111111111111"
	testNodeID    = "22222222-2222-4222-8222-222222222222"
)

func TestNewCanonicalizesAndValidatesIdentity(t *testing.T) {
	createdAt := time.Date(2026, 7, 16, 12, 0, 0, 0, time.FixedZone("offset", 3600))
	identity, err := New(strings.ToUpper(testClusterID), strings.ToUpper(testNodeID), "node-1", []string{RoleWorker, RoleControlPlane, RoleWorker}, createdAt)
	if err != nil {
		t.Fatalf("New returned error: %v", err)
	}
	if identity.ClusterID != testClusterID || identity.NodeID != testNodeID {
		t.Fatalf("IDs were not canonicalized: %#v", identity)
	}
	wantRoles := []string{RoleControlPlane, RoleWorker}
	if strings.Join(identity.EnrollmentRoles, ",") != strings.Join(wantRoles, ",") {
		t.Fatalf("roles = %v, want %v", identity.EnrollmentRoles, wantRoles)
	}
	if identity.CreatedAt.Location() != time.UTC {
		t.Fatalf("createdAt location = %v, want UTC", identity.CreatedAt.Location())
	}
	if !identity.Matches(testClusterID, testNodeID) {
		t.Fatal("identity should match its immutable IDs")
	}
	if identity.Matches(testClusterID, "33333333-3333-4333-8333-333333333333") {
		t.Fatal("identity matched the wrong node ID")
	}
}

func TestNewGeneratesDistinctUUIDs(t *testing.T) {
	first, err := New("", "", "node-1", []string{RoleWorker}, time.Now())
	if err != nil {
		t.Fatalf("first New returned error: %v", err)
	}
	second, err := New("", "", "node-2", []string{RoleWorker}, time.Now())
	if err != nil {
		t.Fatalf("second New returned error: %v", err)
	}
	if first.ClusterID == second.ClusterID || first.NodeID == second.NodeID {
		t.Fatalf("generated IDs collided: first=%#v second=%#v", first, second)
	}
	if !isUUID(first.ClusterID) || !isUUID(first.NodeID) {
		t.Fatalf("generated IDs are not UUIDs: %#v", first)
	}
}

func TestCreateReadAndRefuseReplacement(t *testing.T) {
	identity, err := New(testClusterID, testNodeID, "node-1", []string{RoleWorker, RoleControlPlane}, time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "etc", "tako", "identity.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Create(path, *identity); err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Fatalf("identity mode = %o, want 600", got)
	}
	read, err := Read(path)
	if err != nil {
		t.Fatalf("Read returned error: %v", err)
	}
	if !read.Matches(testClusterID, testNodeID) || !read.HasEnrollmentRole(RoleControlPlane) {
		t.Fatalf("unexpected read identity: %#v", read)
	}
	if err := Create(path, *identity); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("replacement error = %v, want already exists", err)
	}
}

func TestReadRejectsInsecureUnknownAndSymlinkFiles(t *testing.T) {
	identity, err := New(testClusterID, testNodeID, "node-1", []string{RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	insecure := filepath.Join(dir, "insecure.json")
	if err := os.WriteFile(insecure, data, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(insecure); err == nil || !strings.Contains(err.Error(), "group or other") {
		t.Fatalf("insecure file error = %v", err)
	}

	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, append(data[:len(data)-1], []byte(`,"unexpected":true}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(unknown); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}

	symlink := filepath.Join(dir, "identity-link.json")
	if err := os.Symlink(unknown, symlink); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Read(symlink); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestReadRejectsOversizedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	if err := os.WriteFile(path, bytes.Repeat([]byte{'x'}, maxFileSize+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized identity error = %v", err)
	}
}

func TestReadOptionalReturnsNilOnlyForMissingIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	identity, err := ReadOptional(path)
	if err != nil || identity != nil {
		t.Fatalf("ReadOptional = %#v, %v", identity, err)
	}
	if err := os.WriteFile(path, []byte("not-json"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadOptional(path); err == nil {
		t.Fatal("ReadOptional should reject an invalid existing identity")
	}
}

func TestCreateRejectsUnprotectedIdentityDirectory(t *testing.T) {
	identity, err := New(testClusterID, testNodeID, "node-1", []string{RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "writable")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := Create(filepath.Join(dir, "identity.json"), *identity); err == nil || !strings.Contains(err.Error(), "writable by group or other") {
		t.Fatalf("Create error = %v, want unprotected directory rejection", err)
	}
}

func TestCreateRejectsSymlinkedIdentityDirectory(t *testing.T) {
	identity, err := New(testClusterID, testNodeID, "node-1", []string{RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := Create(filepath.Join(linkedDir, "identity.json"), *identity); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Create error = %v, want symlinked directory rejection", err)
	}
}

func TestValidateRejectsUnsupportedIdentityFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Installation)
		want   string
	}{
		{name: "api version", mutate: func(i *Installation) { i.APIVersion = "v0" }, want: "apiVersion"},
		{name: "cluster id", mutate: func(i *Installation) { i.ClusterID = "local" }, want: "clusterId"},
		{name: "node id", mutate: func(i *Installation) { i.NodeID = "node" }, want: "nodeId"},
		{name: "node name", mutate: func(i *Installation) { i.NodeName = "../node" }, want: "nodeName"},
		{name: "role", mutate: func(i *Installation) { i.EnrollmentRoles = []string{"tenant"} }, want: "role"},
		{name: "role whitespace", mutate: func(i *Installation) { i.EnrollmentRoles = []string{" worker "} }, want: "whitespace"},
		{name: "created", mutate: func(i *Installation) { i.CreatedAt = time.Time{} }, want: "createdAt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			identity, err := New(testClusterID, testNodeID, "node-1", []string{RoleWorker}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(identity)
			if err := identity.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want %q", err, test.want)
			}
		})
	}
}
