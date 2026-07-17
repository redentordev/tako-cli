package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

type fakeHost struct {
	units             map[string]string
	activated         []string
	uid               int
	gid               int
	err               error
	stagedSource      string
	stagedDestination string
	readyCheck        ReadinessCheck
	accountCalls      int
	accessGroup       string
	readyErr          error
}

func (h *fakeHost) ResolveDockerDataRoot(_ context.Context, requested string) (string, error) {
	if h.err != nil {
		return "", h.err
	}
	if requested != "" {
		return requested, nil
	}
	return "/var/lib/docker", nil
}

func (h *fakeHost) EnsurePlatformAccounts(_ context.Context, _ string, _ string, accessGroup string) (PlatformAccountIDs, error) {
	h.accountCalls++
	h.accessGroup = accessGroup
	return PlatformAccountIDs{WorkerUID: h.uid, WorkerGID: h.gid, SocketGroupGID: h.gid}, h.err
}

func (h *fakeHost) StageBinary(_ context.Context, source string, destination string) error {
	h.stagedSource, h.stagedDestination = source, destination
	return h.err
}

func (h *fakeHost) InstallUnit(_ context.Context, name string, content string) error {
	if h.err != nil {
		return h.err
	}
	if h.units == nil {
		h.units = make(map[string]string)
	}
	h.units[name] = content
	return nil
}

func (h *fakeHost) ReloadEnableRestart(_ context.Context, names ...string) error {
	h.activated = append([]string(nil), names...)
	return h.err
}

func (h *fakeHost) WaitReady(_ context.Context, check ReadinessCheck) error {
	h.readyCheck = check
	return h.readyErr
}

func TestBootstrapCreatesAndResumesProtectedFirstNode(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{uid: os.Geteuid(), gid: os.Getegid()}
	bootstrapper, err := NewBootstrapper(host)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	config := BootstrapConfig{
		RootDir: root, NodeName: "node-1", BinaryPath: "/usr/local/bin/tako",
		RequireRoot: false, Now: func() time.Time { return now },
	}
	result, err := bootstrapper.Bootstrap(context.Background(), config)
	if err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}
	if result.Resumed || result.State.ControllerMode != "single-writer" {
		t.Fatalf("unexpected bootstrap result: %#v", result)
	}
	if got := strings.Join(result.State.EnrollmentRoles, ","); got != "builder,control-plane,edge,worker" {
		t.Fatalf("roles = %q", got)
	}
	identityPath := filepath.Join(root, "etc", "tako", "identity.json")
	installation, err := nodeidentity.Read(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !installation.Matches(result.State.ClusterID, result.State.NodeID) {
		t.Fatal("bootstrap result and identity do not match")
	}
	for name, required := range map[string]string{
		TakodUnitName:  "RuntimeDirectoryMode=0750",
		WorkerUnitName: "SupplementaryGroups=tako",
	} {
		if !strings.Contains(host.units[name], required) {
			t.Fatalf("unit %s missing %q:\n%s", name, required, host.units[name])
		}
	}
	if strings.Join(host.activated, ",") != TakodUnitName+","+WorkerUnitName {
		t.Fatalf("activated units = %v", host.activated)
	}
	if host.stagedDestination != DefaultBinaryPath || host.readyCheck.NodeID != result.State.NodeID {
		t.Fatalf("binary/readiness not enforced: staged=%q ready=%#v", host.stagedDestination, host.readyCheck)
	}
	if host.accessGroup != DefaultSocketGroup {
		t.Fatalf("bootstrap used socket access group %q", host.accessGroup)
	}
	if result.State.DockerDataRoot != "/var/lib/docker" || !strings.Contains(host.units[TakodUnitName], "--docker-data-root /var/lib/docker") {
		t.Fatalf("Docker data root was not persisted in bootstrap/unit: %#v", result.State)
	}
	for _, path := range []string{
		filepath.Join(root, "var", "lib", "tako", "platform", DefaultJournalName),
		filepath.Join(root, "var", "log", "tako", DefaultAuditLogName),
		filepath.Join(root, "etc", "tako", "platform.json"),
	} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm()&0007 != 0 {
			t.Fatalf("protected file %s = %#v, %v", path, info, err)
		}
	}
	bindingPath := filepath.Join(root, "etc", "tako", "local-node.json")
	binding, err := nodeidentity.ReadLocalBinding(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	if binding.NodeID != result.State.NodeID || binding.WorkerUID != result.State.WorkerUID {
		t.Fatalf("public local binding = %#v", binding)
	}
	if info, err := os.Stat(bindingPath); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("public local binding mode = %#v, %v", info, err)
	}

	second, err := bootstrapper.Bootstrap(context.Background(), config)
	if err != nil {
		t.Fatalf("resume returned error: %v", err)
	}
	if !second.Resumed || second.State.ClusterID != result.State.ClusterID || second.State.NodeID != result.State.NodeID || !second.State.InitializedAt.Equal(now) {
		t.Fatalf("resume changed identity or initialization: %#v", second)
	}
}

func TestBootstrapRejectsConflictingExistingIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "etc", "tako")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	identity, err := nodeidentity.New("", "", "other-node", []string{nodeidentity.RoleWorker}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := nodeidentity.Create(filepath.Join(dir, "identity.json"), *identity); err != nil {
		t.Fatal(err)
	}
	host := &fakeHost{uid: os.Geteuid(), gid: os.Getegid()}
	bootstrapper, _ := NewBootstrapper(host)
	_, err = bootstrapper.Bootstrap(context.Background(), BootstrapConfig{RootDir: root, NodeName: "node-1", BinaryPath: "/usr/local/bin/tako", RequireRoot: false})
	if err == nil || !strings.Contains(err.Error(), "not the requested first-node") {
		t.Fatalf("Bootstrap error = %v", err)
	}
	if len(host.units) != 0 {
		t.Fatal("services were mutated after identity conflict")
	}
	if host.accountCalls != 0 {
		t.Fatal("host accounts were mutated before identity conflict was rejected")
	}
}

func TestReadConfigDocumentMigratesMissingMembershipPath(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "platform")
	document := ConfigDocument{State: BootstrapState{
		APIVersion: APIVersion, Kind: BootstrapKind, ClusterID: "11111111-1111-4111-8111-111111111111", NodeID: "22222222-2222-4222-8222-222222222222", NodeName: "node-1",
		ControllerMode: "single-writer", EnrollmentRoles: append([]string(nil), firstNodeRoles...), IdentityPath: "/etc/tako/identity.json", InventoryPath: "/etc/tako/cluster-inventory.json",
		StateDir: stateDir, AuditDir: "/var/log/tako", SocketPath: DefaultSocket, WorkerSocketPath: DefaultWorkerSocket, DockerDataRoot: "/var/lib/docker", SocketGroup: DefaultSocketGroup,
		ServiceBinaryPath: DefaultBinaryPath, WorkerUser: DefaultWorkerUser, WorkerGroup: DefaultWorkerGroup, WorkerUID: 1001, WorkerGID: 1001, SocketGroupGID: 1002, InitializedAt: time.Now().UTC(),
	}, Policy: DefaultResourcePolicy()}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "platform.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := readConfigDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.State.MembershipPath != DefaultMembershipPath(stateDir) {
		t.Fatalf("migrated membership path = %q", loaded.State.MembershipPath)
	}
}

func TestBootstrapConfigRejectsSharedWorkerAndSocketGroup(t *testing.T) {
	config := BootstrapConfig{NodeName: "node-1", BinaryPath: "/usr/local/bin/tako", WorkerGroup: DefaultSocketGroup}
	config = config.withDefaults()
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "distinct") {
		t.Fatalf("shared worker/socket group was accepted: %v", err)
	}
}

func TestBootstrapResumePreservesExistingNodeAccountsAndPolicy(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{uid: os.Geteuid(), gid: os.Getegid()}
	bootstrapper, _ := NewBootstrapper(host)
	policy := DefaultResourcePolicy()
	policy.MaximumConcurrentBuilds = 3
	first, err := bootstrapper.Bootstrap(context.Background(), BootstrapConfig{
		RootDir: root, NodeName: "prod-a", BinaryPath: "/tmp/tako-source",
		WorkerUser: "custom-worker", WorkerGroup: "custom-worker", Policy: policy, RequireRoot: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := bootstrapper.Bootstrap(context.Background(), BootstrapConfig{
		RootDir: root, BinaryPath: "/tmp/tako-source", RequireRoot: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Resumed || second.State.NodeName != "prod-a" || second.State.WorkerUser != "custom-worker" || second.Policy.MaximumConcurrentBuilds != 3 {
		t.Fatalf("resume changed persisted bootstrap settings: first=%#v second=%#v", first, second)
	}
}

func TestBootstrapRejectsImplicitReconfigurationBeforeHostMutation(t *testing.T) {
	root := t.TempDir()
	firstHost := &fakeHost{uid: os.Geteuid(), gid: os.Getegid()}
	bootstrapper, _ := NewBootstrapper(firstHost)
	_, err := bootstrapper.Bootstrap(context.Background(), BootstrapConfig{
		RootDir: root, NodeName: "prod-a", BinaryPath: "/tmp/tako-source", RequireRoot: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondHost := &fakeHost{uid: os.Geteuid(), gid: os.Getegid()}
	bootstrapper, _ = NewBootstrapper(secondHost)
	_, err = bootstrapper.Bootstrap(context.Background(), BootstrapConfig{
		RootDir: root, BinaryPath: "/tmp/tako-source", WorkerUser: "different-worker", WorkerUserExplicit: true, RequireRoot: false,
	})
	if err == nil || !strings.Contains(err.Error(), "reconfigure") {
		t.Fatalf("bootstrap reconfiguration error = %v", err)
	}
	if secondHost.accountCalls != 0 || secondHost.stagedDestination != "" || len(secondHost.units) != 0 {
		t.Fatalf("host mutated before reconfiguration rejection: %#v", secondHost)
	}
}

func TestBootstrapFailsWhenServicesDoNotBecomeReady(t *testing.T) {
	root := t.TempDir()
	host := &fakeHost{uid: os.Geteuid(), gid: os.Getegid(), readyErr: context.DeadlineExceeded}
	bootstrapper, _ := NewBootstrapper(host)
	_, err := bootstrapper.Bootstrap(context.Background(), BootstrapConfig{
		RootDir: root, NodeName: "node-1", BinaryPath: "/tmp/tako-source", RequireRoot: false,
	})
	if err == nil || !strings.Contains(err.Error(), "service readiness") {
		t.Fatalf("bootstrap readiness error = %v", err)
	}
	auditData, readErr := os.ReadFile(filepath.Join(root, "var", "log", "tako", DefaultAuditLogName))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(auditData), "first controller initialized") {
		t.Fatal("bootstrap wrote a success audit before service readiness")
	}
}

func TestJournalProducesDurableJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.jsonl")
	journal, _ := NewJournal(path)
	record := OperationRecord{OperationID: "op-1", Operation: "deploy", Phase: "planned", Status: "completed", Timestamp: time.Now()}
	if err := journal.Append(record); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded OperationRecord
	if err := json.Unmarshal(data, &decoded); err != nil || decoded.OperationID != record.OperationID {
		t.Fatalf("journal record = %#v, %v", decoded, err)
	}
}

func TestJournalRepairsTornFinalFrameBeforeAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournalName)
	valid := OperationRecord{OperationID: "one", Operation: "test", Phase: "done", Status: "completed", Timestamp: time.Now().UTC()}
	encoded, _ := json.Marshal(valid)
	data := append(append(encoded, '\n'), []byte(`{"operationId":"torn"`)...)
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	journal, _ := NewJournal(path)
	second := OperationRecord{OperationID: "two", Operation: "test", Phase: "done", Status: "completed", Timestamp: time.Now().UTC()}
	if err := journal.Append(second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("journal lines = %q", data)
	}
	for _, line := range lines {
		var record OperationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("journal contains torn frame: %v", err)
		}
	}
}

func TestJournalSerializesTornRepairAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournalName)
	if err := os.WriteFile(path, []byte(`{"operationId":"torn"`), 0640); err != nil {
		t.Fatal(err)
	}
	const writers = 12
	start := make(chan struct{})
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for index := range writers {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			journal, _ := NewJournal(path)
			errs <- journal.Append(OperationRecord{OperationID: fmt.Sprintf("op-%d", index), Operation: "test", Phase: "done", Status: "completed", Timestamp: time.Now().UTC()})
		}(index)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, _ := os.ReadFile(path)
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	if len(lines) != writers {
		t.Fatalf("journal lost a concurrent append: got %d lines, want %d", len(lines), writers)
	}
	for _, line := range lines {
		if !json.Valid(line) {
			t.Fatalf("journal contains invalid frame: %q", line)
		}
	}
}

func TestJournalRejectsOversizedFrameAndUnboundedTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), DefaultJournalName)
	journal, _ := NewJournal(path)
	oversized := OperationRecord{OperationID: "large", Operation: "test", Phase: "done", Status: "failed", Message: strings.Repeat("x", int(maxJournalFrameBytes)), Timestamp: time.Now().UTC()}
	if err := journal.Append(oversized); err == nil || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("oversized journal frame was accepted: %v", err)
	}
	valid := OperationRecord{OperationID: "valid", Operation: "test", Phase: "done", Status: "completed", Timestamp: time.Now().UTC()}
	line, _ := json.Marshal(valid)
	data := append(append(line, '\n'), bytes.Repeat([]byte{'x'}, int(maxJournalFrameBytes)+1)...)
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
	if err := journal.Append(valid); err == nil || !strings.Contains(err.Error(), "recovery limit") {
		t.Fatalf("unbounded torn tail was silently scanned/truncated: %v", err)
	}
}

func TestEnsureOwnedDurableFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("protected"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "operations.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureOwnedDurableFile(link, os.Geteuid(), os.Getegid()); err == nil {
		t.Fatal("durable file initializer followed a symlink")
	}
	data, err := os.ReadFile(target)
	if err != nil || string(data) != "protected" {
		t.Fatalf("symlink target changed to %q: %v", data, err)
	}
}

func TestJournalAppendRejectsSymlinkReplacement(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("protected"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, DefaultJournalName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	journal, _ := NewJournal(link)
	err := journal.Append(OperationRecord{OperationID: "op", Operation: "test", Phase: "write", Status: "running", Timestamp: time.Now()})
	if err == nil {
		t.Fatal("journal append followed a replacement symlink")
	}
	data, readErr := os.ReadFile(target)
	if readErr != nil || string(data) != "protected" {
		t.Fatalf("symlink target changed to %q: %v", data, readErr)
	}
}
