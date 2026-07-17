package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestPrepareWorkerIdentityIsCreateOnceAndWorkerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "etc", "tako", "identity.json")
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	first, resumed, err := PrepareWorkerIdentity(path, "11111111-1111-4111-8111-111111111111", membershipWorkerID, "worker-2", now)
	if err != nil || resumed {
		t.Fatalf("first prepare = %#v resumed=%t err=%v", first, resumed, err)
	}
	second, resumed, err := PrepareWorkerIdentity(path, first.ClusterID, first.NodeID, first.NodeName, now.Add(time.Hour))
	if err != nil || !resumed || second.AllocationPublicKey != first.AllocationPublicKey {
		t.Fatalf("resume changed worker identity: first=%#v second=%#v resumed=%t err=%v", first, second, resumed, err)
	}
	installation, err := nodeidentity.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(installation.EnrollmentRoles) != 1 || installation.EnrollmentRoles[0] != nodeidentity.RoleWorker {
		t.Fatalf("candidate gained elevated enrollment roles: %v", installation.EnrollmentRoles)
	}
	if _, _, err := PrepareWorkerIdentity(path, first.ClusterID, "44444444-4444-4444-8444-444444444444", "worker-3", now); err == nil || !strings.Contains(err.Error(), "different") {
		t.Fatalf("identity replacement error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("identity mode=%v", info.Mode())
	}
}
