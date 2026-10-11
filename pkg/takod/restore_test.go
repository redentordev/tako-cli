package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

type retainFixture struct {
	logPath      string
	dataDir      string
	liveName     string
	retainedName string
}

func retainInspectLine(name string, running bool, policy string, labels map[string]string) string {
	encoded, _ := json.Marshal(labels)
	return fmt.Sprintf("/%s|%t|%s|%s", name, running, policy, encoded)
}

func serviceLabels(slot string, revision string) map[string]string {
	return map[string]string{
		"tako.project":     "demo",
		"tako.environment": "production",
		"tako.service":     "web",
		"tako.slot":        slot,
		"tako.revision":    revision,
	}
}

// newRetainFixture fakes one running previous container (old1) in slot 1
// with restart policy "always".
func newRetainFixture(t *testing.T) retainFixture {
	t.Helper()
	fixture := retainFixture{
		logPath:      filepath.Join(t.TempDir(), "commands.log"),
		dataDir:      t.TempDir(),
		liveName:     runtimeid.ContainerName("demo", "production", "web", 1),
		retainedName: runtimeid.RetainedContainerName("demo", "production", "web", 1),
	}
	restore := useFakeCommands(t, fixture.logPath)
	t.Cleanup(restore)
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "old1\n")
	inspect, _ := json.Marshal(map[string]string{
		"old1": retainInspectLine(fixture.liveName, true, "always", serviceLabels("1", "rev-old")),
	})
	t.Setenv("TAKO_FAKE_RETAIN_INSPECT_BY_ID", string(inspect))
	return fixture
}

func (f retainFixture) request() ReconcileServiceRequest {
	return ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Revision:       "rev-new",
		DeployStrategy: "recreate",
		FailurePolicy:  FailurePolicyRestore,
		Image:          "registry.example.com/demo/web:new",
		Network:        "tako_demo_production",
		Restart:        "always",
		DataDir:        f.dataDir,
		Containers: []ContainerSpec{{
			Name:   f.liveName,
			Labels: map[string]string{"tako.slot": "1", "tako.revision": "rev-new"},
		}},
	}
}

func unhealthyCommandHealth(t *testing.T) *HealthSpec {
	t.Helper()
	t.Setenv("TAKO_FAKE_HEALTH_STATUS", "unhealthy")
	return &HealthSpec{Command: "test -f /tmp/healthy", WaitAttempts: 1}
}

func indexOfEntry(entries []string, want string) int {
	for index, entry := range entries {
		if entry == want {
			return index
		}
	}
	return -1
}

func assertEntryOrder(t *testing.T, entries []string, ordered ...string) {
	t.Helper()
	last := -1
	for _, want := range ordered {
		index := indexOfEntry(entries, want)
		if index < 0 {
			t.Fatalf("docker log missing %q in %#v", want, entries)
		}
		if index <= last {
			t.Fatalf("docker log entry %q out of order in %#v", want, entries)
		}
		last = index
	}
}

func TestReconcileServiceRestorePolicyRetainsPreviousContainers(t *testing.T) {
	fixture := newRetainFixture(t)

	response, err := ReconcileService(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}
	if response.RestoreToken == "" || !strings.HasPrefix(response.RestoreToken, "rev-new.") {
		t.Fatalf("restore token = %q, want revision-scoped token", response.RestoreToken)
	}
	if !slices.Equal(response.RetainedContainers, []string{fixture.retainedName}) {
		t.Fatalf("retained containers = %#v, want %q", response.RetainedContainers, fixture.retainedName)
	}

	entries := readCommandLog(t, fixture.logPath)
	assertEntryOrder(t, entries,
		"docker stop old1",
		"docker update --restart no old1",
		"docker rename old1 "+fixture.retainedName,
	)
	if index := indexOfEntry(entries, "docker rename old1 "+fixture.retainedName); index > slices.IndexFunc(entries, func(entry string) bool { return strings.HasPrefix(entry, "docker run ") }) {
		t.Fatalf("previous container must be renamed before the new one starts: %#v", entries)
	}
	for _, entry := range entries {
		if entry == "docker rm old1" || entry == "docker rm "+fixture.retainedName {
			t.Fatalf("restore policy removed the previous container: %#v", entries)
		}
	}
	state, err := readRetainedState(fixture.dataDir, "demo", "production", "web")
	if err != nil || state == nil {
		t.Fatalf("retained state = %#v, %v; want persisted record", state, err)
	}
	if state.Token != response.RestoreToken || !slices.Equal(state.Started, []string{fixture.liveName}) {
		t.Fatalf("retained state = %#v, want token and started container", state)
	}

	// Finalize removes the retained container exactly once.
	request := ServiceRecoveryRequest{Project: "demo", Environment: "production", Service: "web", Revision: "rev-new", Token: response.RestoreToken}
	finalized, err := FinalizeRetainedService(context.Background(), fixture.dataDir, request)
	if err != nil {
		t.Fatalf("FinalizeRetainedService returned error: %v", err)
	}
	if finalized.Status != ServiceRecoveryFinalized || !slices.Equal(finalized.Containers, []string{fixture.retainedName}) {
		t.Fatalf("finalize response = %#v", finalized)
	}
	entries = readCommandLog(t, fixture.logPath)
	assertEntryOrder(t, entries, "docker stop "+fixture.retainedName, "docker rm "+fixture.retainedName)
	again, err := FinalizeRetainedService(context.Background(), fixture.dataDir, request)
	if err != nil || again.Status != ServiceRecoveryNoop {
		t.Fatalf("repeated finalize = %#v, %v; want no-op", again, err)
	}
	if after := readCommandLog(t, fixture.logPath); len(after) != len(entries) {
		t.Fatalf("repeated finalize issued docker commands: %#v", after[len(entries):])
	}
}

func TestReconcileServiceRestorePolicySweepsOnlyRecomputedLeftovers(t *testing.T) {
	fixture := newRetainFixture(t)
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "left1\nold1\nimpost1\n")
	inspect, _ := json.Marshal(map[string]string{
		"left1": retainInspectLine(fixture.retainedName, false, "no", serviceLabels("1", "rev-older")),
		"old1":  retainInspectLine(fixture.liveName, true, "always", serviceLabels("1", "rev-old")),
		// Ends in --prev but its labels recompute to a different name.
		"impost1": retainInspectLine("demo_production_web--prev", true, "unless-stopped", serviceLabels("2", "rev-old")),
	})
	t.Setenv("TAKO_FAKE_RETAIN_INSPECT_BY_ID", string(inspect))

	if _, err := ReconcileService(context.Background(), fixture.request()); err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}
	entries := readCommandLog(t, fixture.logPath)
	if !slices.Contains(entries, "docker rm left1") {
		t.Fatalf("interrupted-deploy leftover was not removed: %#v", entries)
	}
	if index := indexOfEntry(entries, "docker rm left1"); index > slices.IndexFunc(entries, func(entry string) bool { return strings.HasPrefix(entry, "docker run ") }) {
		t.Fatalf("leftover must be removed before new containers start: %#v", entries)
	}
	for _, entry := range entries {
		if entry == "docker rm impost1" {
			t.Fatalf("container merely ending in --prev was removed as a leftover: %#v", entries)
		}
	}
	impostRetained := runtimeid.RetainedContainerName("demo", "production", "web", 2)
	if !slices.Contains(entries, "docker rename impost1 "+impostRetained) {
		t.Fatalf("slot-2 container should be retained under its own recomputed name: %#v", entries)
	}
}

func TestReconcileServiceRestorePolicyRestoresOnFailures(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, fixture retainFixture, request *ReconcileServiceRequest)
		// createdOnly marks a docker run failure: the new container never
		// started, but may hold the slot name and is removed anyway.
		want string
	}{
		{
			name: "health check failure",
			setup: func(t *testing.T, _ retainFixture, request *ReconcileServiceRequest) {
				request.Health = unhealthyCommandHealth(t)
			},
			want: "health check",
		},
		{
			name: "start failure",
			setup: func(t *testing.T, fixture retainFixture, _ *ReconcileServiceRequest) {
				t.Setenv("TAKO_FAKE_DOCKER_RUN_FAIL_CONTAINS", "--name "+fixture.liveName)
			},
			want: "failed to start container",
		},
		{
			name: "network connect failure",
			setup: func(t *testing.T, _ retainFixture, request *ReconcileServiceRequest) {
				t.Setenv("TAKO_FAKE_FAIL_NETWORK_CONNECT_ONCE_FILE", filepath.Join(t.TempDir(), "connect-failed"))
				request.NetworkAttachments = []NetworkAttachmentSpec{{Network: "tako_shared_exports"}}
			},
			want: "network",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRetainFixture(t)
			request := fixture.request()
			tc.setup(t, fixture, &request)

			_, err := ReconcileService(context.Background(), request)
			var failure *ReconcileFailure
			if !errors.As(err, &failure) {
				t.Fatalf("ReconcileService error = %v, want *ReconcileFailure", err)
			}
			if !failure.Restored || failure.RestoreError != "" || failure.Policy != FailurePolicyRestore {
				t.Fatalf("failure = %#v, want restored", failure)
			}
			if !strings.Contains(strings.ToLower(failure.Error()), tc.want) {
				t.Fatalf("failure error = %q, want %q", failure.Error(), tc.want)
			}
			entries := readCommandLog(t, fixture.logPath)
			assertEntryOrder(t, entries,
				"docker rename old1 "+fixture.retainedName,
				"docker rm "+fixture.liveName,
				"docker rename "+fixture.retainedName+" "+fixture.liveName,
				"docker update --restart always "+fixture.liveName,
				"docker start "+fixture.liveName,
			)
			if state, err := readRetainedState(fixture.dataDir, "demo", "production", "web"); err != nil || state != nil {
				t.Fatalf("retained state after local restore = %#v, %v; want cleared", state, err)
			}
		})
	}
}

func TestReconcileServiceRestorePolicyReportsRestoreFailure(t *testing.T) {
	fixture := newRetainFixture(t)
	request := fixture.request()
	request.Health = unhealthyCommandHealth(t)
	t.Setenv("TAKO_FAKE_DOCKER_START_ERROR", "port is already allocated")

	_, err := ReconcileService(context.Background(), request)
	var failure *ReconcileFailure
	if !errors.As(err, &failure) {
		t.Fatalf("ReconcileService error = %v, want *ReconcileFailure", err)
	}
	if failure.Restored || !strings.Contains(failure.RestoreError, "start "+fixture.liveName) {
		t.Fatalf("failure = %#v, want restored=false with start error", failure)
	}
	body := failure.Body()
	if body.Restored || body.RestoreError == "" || body.FailurePolicy != FailurePolicyRestore || body.Error == "" {
		t.Fatalf("failure body = %#v", body)
	}
	// The record stays so a retry or an operator can still find the
	// previous containers.
	if state, err := readRetainedState(fixture.dataDir, "demo", "production", "web"); err != nil || state == nil {
		t.Fatalf("retained state after failed restore = %#v, %v; want kept", state, err)
	}
}

func TestReconcileServicePersistentRestoreReusesVolumesAndPublishes(t *testing.T) {
	fixture := newRetainFixture(t)
	request := fixture.request()
	request.Service = "web"
	request.Mounts = []string{"type=volume,source=" + runtimeid.VolumeName("demo", "production", "pgdata") + ",target=/var/lib/postgresql/data"}
	request.Containers[0].Publishes = []string{"5432:5432/tcp"}
	request.Health = unhealthyCommandHealth(t)

	_, err := ReconcileService(context.Background(), request)
	var failure *ReconcileFailure
	if !errors.As(err, &failure) || !failure.Restored {
		t.Fatalf("ReconcileService error = %v, want restored failure", err)
	}
	entries := readCommandLog(t, fixture.logPath)
	failedAt := indexOfEntry(entries, "docker rm "+fixture.liveName)
	if failedAt < 0 {
		t.Fatalf("new container was not removed: %#v", entries)
	}
	for _, entry := range entries[failedAt:] {
		if strings.HasPrefix(entry, "docker volume") || strings.HasPrefix(entry, "docker run ") || strings.HasPrefix(entry, "docker create") {
			t.Fatalf("restore must restart the kept container, not recreate containers or volumes: %q in %#v", entry, entries)
		}
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker volume rm") {
			t.Fatalf("restore removed a volume: %#v", entries)
		}
	}
	assertEntryOrder(t, entries, "docker rename "+fixture.retainedName+" "+fixture.liveName, "docker start "+fixture.liveName)
}

func TestRestoreRetainedServiceRevertsSucceededNode(t *testing.T) {
	fixture := newRetainFixture(t)
	response, err := ReconcileService(context.Background(), fixture.request())
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}
	before := len(readCommandLog(t, fixture.logPath))

	request := ServiceRecoveryRequest{Project: "demo", Environment: "production", Service: "web", Revision: "rev-new", Token: response.RestoreToken}
	restored, err := RestoreRetainedService(context.Background(), fixture.dataDir, request)
	if err != nil {
		t.Fatalf("RestoreRetainedService returned error: %v", err)
	}
	if restored.Status != ServiceRecoveryRestored || !slices.Equal(restored.Containers, []string{fixture.liveName}) {
		t.Fatalf("restore response = %#v", restored)
	}
	entries := readCommandLog(t, fixture.logPath)[before:]
	assertEntryOrder(t, entries,
		"docker stop "+fixture.liveName,
		"docker rm "+fixture.liveName,
		"docker rename "+fixture.retainedName+" "+fixture.liveName,
		"docker start "+fixture.liveName,
	)

	again, err := RestoreRetainedService(context.Background(), fixture.dataDir, request)
	if err != nil || again.Status != ServiceRecoveryNoop {
		t.Fatalf("repeated restore = %#v, %v; want no-op", again, err)
	}
	// A wrong-revision or unknown token never touches containers.
	wrong := request
	wrong.Token = "rev-new.0000000000000000"
	if result, err := FinalizeRetainedService(context.Background(), fixture.dataDir, wrong); err != nil || result.Status != ServiceRecoveryNoop {
		t.Fatalf("unknown-token finalize = %#v, %v; want no-op", result, err)
	}
}

func TestServiceRecoveryHandlers(t *testing.T) {
	fixture := newRetainFixture(t)
	request := fixture.request()
	server := NewServer("/tmp/takod-test.sock", fixture.dataDir, "test")
	request.DataDir = fixture.dataDir
	response, err := ReconcileService(context.Background(), request)
	if err != nil {
		t.Fatalf("ReconcileService returned error: %v", err)
	}

	call := func(handler http.HandlerFunc, path string, body any) (int, ServiceRecoveryResponse) {
		t.Helper()
		encoded, _ := json.Marshal(body)
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded)))
		var decoded ServiceRecoveryResponse
		if recorder.Code == http.StatusOK {
			if err := json.NewDecoder(recorder.Body).Decode(&decoded); err != nil {
				t.Fatalf("decode %s response: %v", path, err)
			}
		}
		return recorder.Code, decoded
	}

	unknown := ServiceRecoveryRequest{Project: "demo", Environment: "production", Service: "web", Revision: "rev-new", Token: "rev-new.ffffffffffffffff"}
	if code, decoded := call(server.handleServiceFinalize, "/v1/services/finalize", unknown); code != http.StatusOK || decoded.Status != ServiceRecoveryNoop {
		t.Fatalf("unknown token finalize = %d %#v, want 200 noop", code, decoded)
	}
	valid := unknown
	valid.Token = response.RestoreToken
	if code, decoded := call(server.handleServiceFinalize, "/v1/services/finalize", valid); code != http.StatusOK || decoded.Status != ServiceRecoveryFinalized {
		t.Fatalf("finalize = %d %#v, want 200 finalized", code, decoded)
	}
	if code, decoded := call(server.handleServiceFinalize, "/v1/services/finalize", valid); code != http.StatusOK || decoded.Status != ServiceRecoveryNoop {
		t.Fatalf("repeated finalize = %d %#v, want 200 noop", code, decoded)
	}
	if code, decoded := call(server.handleServiceRestore, "/v1/services/restore", valid); code != http.StatusOK || decoded.Status != ServiceRecoveryNoop {
		t.Fatalf("restore after finalize = %d %#v, want 200 noop", code, decoded)
	}
	invalid := valid
	invalid.Token = "../escape"
	if code, _ := call(server.handleServiceRestore, "/v1/services/restore", invalid); code != http.StatusBadRequest {
		t.Fatalf("invalid token status = %d, want 400", code)
	}
	recorder := httptest.NewRecorder()
	server.handleServiceFinalize(recorder, httptest.NewRequest(http.MethodGet, "/v1/services/finalize", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET finalize status = %d, want 405", recorder.Code)
	}
}

func TestWriteReconcileFailureReturnsTypedBody(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeReconcileFailure(recorder, &ReconcileFailure{Err: errors.New("health check timeout"), Policy: FailurePolicyRestore, Restored: true})
	if recorder.Code != http.StatusBadGateway || recorder.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status = %d content-type = %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	var body ReconcileFailureBody
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error != "health check timeout" || !body.Restored || body.FailurePolicy != FailurePolicyRestore {
		t.Fatalf("body = %#v", body)
	}
}

func TestReconcileServiceRetainFailedRollingKeepsFailedContainersStopped(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "blue123\n")
	t.Setenv("TAKO_FAKE_INSPECT_LABELS_BY_ID", `{"blue123":"{\"tako.revision\":\"rev-blue\"}","green456":"{\"tako.revision\":\"rev-green\"}"}`)
	newName := runtimeid.RevisionContainerName("demo", "production", "web", "rev-green", 1)
	request := ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Revision:       "rev-green",
		DeployStrategy: "rolling",
		FailurePolicy:  FailurePolicyRetainFailed,
		Image:          "registry.example.com/demo/web:green",
		Network:        "tako_demo_production",
		Containers:     []ContainerSpec{{Name: newName, Labels: map[string]string{"tako.revision": "rev-green", "tako.slot": "1"}}},
		Health:         unhealthyCommandHealth(t),
	}

	_, err := ReconcileService(context.Background(), request)
	var failure *ReconcileFailure
	if !errors.As(err, &failure) || failure.Restored || !slices.Equal(failure.KeptContainers, []string{newName}) {
		t.Fatalf("ReconcileService error = %#v (%v), want retain-failed failure keeping %q", failure, err, newName)
	}
	entries := readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker stop "+newName) {
		t.Fatalf("failed container was not stopped: %#v", entries)
	}
	for _, entry := range entries {
		if entry == "docker rm "+newName || strings.Contains(entry, "blue123") && (strings.HasPrefix(entry, "docker stop") || strings.HasPrefix(entry, "docker rm")) {
			t.Fatalf("retain-failed removed the failed container or touched the old revision: %#v", entries)
		}
	}

	// The next reconcile of the same revision removes the kept container.
	if err := os.Remove(logPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "blue123\ngreen456\n")
	t.Setenv("TAKO_FAKE_HEALTH_STATUS", "healthy")
	request.FailurePolicy = ""
	if _, err := ReconcileService(context.Background(), request); err != nil {
		t.Fatalf("follow-up ReconcileService returned error: %v", err)
	}
	entries = readCommandLog(t, logPath)
	if !slices.Contains(entries, "docker rm green456") {
		t.Fatalf("same-revision reconcile did not remove the kept failed container: %#v", entries)
	}
	for _, entry := range entries {
		if entry == "docker rm blue123" {
			t.Fatalf("same-revision reconcile removed the serving revision: %#v", entries)
		}
	}
}

func TestReconcileServiceRetainFailedRecreateRemovesOldThenKeepsFailed(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "old1\n")
	liveName := runtimeid.ContainerName("demo", "production", "web", 1)
	dataDir := t.TempDir()
	request := ReconcileServiceRequest{
		DataDir:        dataDir,
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		DeployStrategy: "recreate",
		FailurePolicy:  FailurePolicyRetainFailed,
		Image:          "registry.example.com/demo/web:new",
		Network:        "tako_demo_production",
		Containers:     []ContainerSpec{{Name: liveName}},
		Health:         unhealthyCommandHealth(t),
	}
	_, err := ReconcileService(context.Background(), request)
	var failure *ReconcileFailure
	if !errors.As(err, &failure) || failure.Restored || failure.Policy != FailurePolicyRetainFailed {
		t.Fatalf("ReconcileService error = %v, want retain-failed failure", err)
	}
	// The kept container is recorded so disk hygiene never reclaims it.
	kept, err := readRetainedFailedContainers(dataDir, "demo", "production")
	if err != nil || !kept[liveName] {
		t.Fatalf("retain-failed record = %v err = %v, want %s", kept, err, liveName)
	}
	defer func() {
		// The next recreate reconcile removes the kept container and clears
		// the record.
		t.Setenv("TAKO_FAKE_HEALTH_STATUS", "healthy")
		request.FailurePolicy = ""
		request.Health = nil
		if _, err := ReconcileService(context.Background(), request); err != nil {
			t.Fatalf("follow-up ReconcileService returned error: %v", err)
		}
		if kept, err := readRetainedFailedContainers(dataDir, "demo", "production"); err != nil || len(kept) != 0 {
			t.Fatalf("retain-failed record after redeploy = %v err = %v", kept, err)
		}
	}()
	entries := readCommandLog(t, logPath)
	assertEntryOrder(t, entries, "docker stop old1", "docker rm old1", "docker stop "+liveName)
	if slices.Contains(entries, "docker rm "+liveName) {
		t.Fatalf("retain-failed removed the failed container: %#v", entries)
	}
	if strings.Contains(strings.Join(entries, "\n"), "docker rename") {
		t.Fatalf("retain-failed must not retain the previous container: %#v", entries)
	}
}

func TestReconcileServiceLegacyPolicyUnchanged(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PS_OUTPUT", "old1\n")
	liveName := runtimeid.ContainerName("demo", "production", "web", 1)
	_, err := ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Image:       "registry.example.com/demo/web:new",
		Network:     "tako_demo_production",
		Containers:  []ContainerSpec{{Name: liveName}},
		Health:      unhealthyCommandHealth(t),
	})
	if err == nil {
		t.Fatal("ReconcileService should fail on unhealthy container")
	}
	var failure *ReconcileFailure
	if errors.As(err, &failure) {
		t.Fatalf("legacy policy must return the untyped error, got %#v", failure)
	}
	entries := readCommandLog(t, logPath)
	assertEntryOrder(t, entries, "docker stop old1", "docker rm old1", "docker stop "+liveName, "docker rm "+liveName)
}

func TestReconcileServiceImplicitTCPReadinessFailureNamesProbe(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	_ = listener.Close() // closed port: every dial is refused

	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_CONTAINER_IP", "127.0.0.1")

	_, err = ReconcileService(context.Background(), ReconcileServiceRequest{
		Project:        "demo",
		Environment:    "production",
		Service:        "web",
		Revision:       "rev-new",
		DeployStrategy: "rolling",
		Image:          "registry.example.com/demo/web:new",
		Network:        "tako_demo_production",
		Containers:     []ContainerSpec{{Name: "demo_production_web_r_rev_new_1", Labels: map[string]string{"tako.revision": "rev-new"}}},
		Health:         &HealthSpec{Port: mustAtoi(t, port), Interval: "2s", Timeout: "2s", Retries: 1, WaitAttempts: 1, Implicit: true},
	})
	if err == nil {
		t.Fatal("ReconcileService should fail when the implicit TCP probe never connects")
	}
	for _, want := range []string{"implicit TCP readiness probe", "port " + port, "healthCheck", "deploy.strategy: recreate"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	}
}
