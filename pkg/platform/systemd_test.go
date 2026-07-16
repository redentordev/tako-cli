package platform

import (
	"strings"
	"testing"
)

func TestPlatformUnitsCarryAdmissionAndResourcePolicy(t *testing.T) {
	policy := DefaultResourcePolicy()
	takodUnit, err := RenderTakodUnit(
		"/usr/local/bin/tako", DefaultSocket, DefaultStateDir, "node-1", "/etc/tako/identity.json", "/srv/docker", policy,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"--minimum-free-disk-bytes 10737418240",
		"--max-concurrent-builds 1",
		"MemoryMin=536870912",
		"OOMScoreAdjust=-500",
		"RuntimeDirectoryMode=0750",
		"Group=tako",
		"/etc/tako/proxy",
		"/var/log/tako/proxy",
		"--docker-data-root /srv/docker",
	} {
		if !strings.Contains(takodUnit, required) {
			t.Fatalf("takod unit missing %q:\n%s", required, takodUnit)
		}
	}

	workerUnit, err := RenderWorkerUnit(BootstrapConfig{NodeName: "node-1", BinaryPath: "/usr/local/bin/tako", Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"MemoryLow=536870912", "TasksMax=256", "SupplementaryGroups=tako"} {
		if !strings.Contains(workerUnit, required) {
			t.Fatalf("worker unit missing %q:\n%s", required, workerUnit)
		}
	}
}

func TestRenderTakodUnitRejectsSystemdArgumentInjection(t *testing.T) {
	for _, nodeName := range []string{"node one", "node%h", "node\nExecStart=/bin/false"} {
		if _, err := RenderTakodUnit(
			"/usr/local/bin/tako", DefaultSocket, DefaultStateDir, nodeName, "/etc/tako/identity.json", "/var/lib/docker", DefaultResourcePolicy(),
		); err == nil {
			t.Fatalf("node name %q was accepted", nodeName)
		}
	}
	if _, err := RenderWorkerUnit(BootstrapConfig{NodeName: "node-1", BinaryPath: "/usr/local/bin/tako", ServiceBinaryPath: "/usr/local/lib/tako/tako%h"}); err == nil {
		t.Fatal("worker unit accepted a systemd specifier in the binary path")
	}
}
