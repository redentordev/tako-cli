package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/runtimeid"
)

func testPreviewRecordJSON(t *testing.T, project string, environment string) string {
	t.Helper()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	expires := now.Add(168 * time.Hour)
	data, err := json.Marshal(PreviewRecord{
		APIVersion: PreviewRecordAPIVersion, Kind: PreviewRecordKind,
		Project: project, Environment: environment,
		Name: strings.TrimPrefix(environment, PreviewEnvironmentPrefix), Base: "preview",
		Servers: []string{"web1"}, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, TTL: "168h",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func writeTestPreviewRecord(t *testing.T, dataDir string, project string, environment string) {
	t.Helper()
	if _, err := WriteStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project: project, Environment: environment, Document: stateDocumentPreview,
		Content: testPreviewRecordJSON(t, project, environment),
	}); err != nil {
		t.Fatalf("write preview record: %v", err)
	}
}

func writeTestFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func previewTeardownRequest(environment string) CleanupRequest {
	return CleanupRequest{
		Project: "demo", Environment: environment,
		RemoveContainers: true, RemoveNetworks: true, RemoveTakodState: true, RemovePreviewData: true,
	}
}

func TestCleanupNetworksMatchesEnvironmentByExactName(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	base := runtimeid.NetworkName("demo", "preview")
	baseExport := runtimeid.ExportNetworkName("demo", "preview", "api")
	child := runtimeid.NetworkName("demo", "preview-pr-42")
	childExport := runtimeid.ExportNetworkName("demo", "preview-pr-42", "api")
	legacyExport := runtimeid.ExportNetworkName("demo", "preview", "worker")
	t.Setenv("TAKO_FAKE_NETWORK_LS_OUTPUT", strings.Join([]string{
		base + "\t<no value>\t<no value>\t<no value>",
		baseExport + "\tdemo\tpreview\tapi",
		child + "\t<no value>\t<no value>\t<no value>",
		childExport + "\tdemo\tpreview-pr-42\tapi",
		legacyExport + "\t<no value>\t<no value>\t<no value>",
		// A labeled network whose name does not match its service label.
		"forged-network\tdemo\tpreview\tapi",
	}, "\n")+"\n")

	removed, err := cleanupNetworks(context.Background(), "demo", "preview")
	if err != nil {
		t.Fatalf("cleanupNetworks returned error: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	entries := readCommandLog(t, logPath)
	for _, want := range []string{"docker network rm " + base, "docker network rm " + baseExport} {
		if !slices.Contains(entries, want) {
			t.Fatalf("missing %q in %#v", want, entries)
		}
	}
	for _, entry := range entries {
		for _, untouched := range []string{child, childExport, legacyExport, "forged-network"} {
			if strings.HasSuffix(entry, " "+untouched) || strings.Contains(entry, " "+untouched+" ") {
				t.Fatalf("cleanup of preview touched %s via %q", untouched, entry)
			}
		}
	}
}

func TestCleanupNetworksForProdKeepsProdEUNetworks(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	prod := runtimeid.NetworkName("demo", "prod")
	prodEU := runtimeid.NetworkName("demo", "prod-eu")
	prodEUExport := runtimeid.ExportNetworkName("demo", "prod-eu", "api")
	t.Setenv("TAKO_FAKE_NETWORK_LS_OUTPUT", strings.Join([]string{
		prod + "\t\t\t",
		prodEU + "\t\t\t",
		prodEUExport + "\tdemo\tprod-eu\tapi",
	}, "\n")+"\n")
	removed, err := cleanupNetworks(context.Background(), "demo", "prod")
	if err != nil || removed != 1 {
		t.Fatalf("cleanupNetworks = %d, %v; want 1, nil", removed, err)
	}
	for _, entry := range readCommandLog(t, logPath) {
		if strings.Contains(entry, prodEU) {
			t.Fatalf("destroying prod touched prod-eu via %q", entry)
		}
	}
}

func TestPreviewTeardownRequestRejectsSharedResourceRemoval(t *testing.T) {
	cases := map[string]func(*CleanupRequest){
		"non-preview environment": func(r *CleanupRequest) { r.Environment = "production" },
		"images":                  func(r *CleanupRequest) { r.RemoveImages = true },
		"image repositories":      func(r *CleanupRequest) { r.ImageRepositories = []string{"demo/web"} },
		"deploy files":            func(r *CleanupRequest) { r.RemoveDeployFiles = true },
		"old images":              func(r *CleanupRequest) { r.CleanOldImages = true },
		"docker prune":            func(r *CleanupRequest) { r.PruneDocker = true },
		"unused volumes":          func(r *CleanupRequest) { r.CleanUnusedVolumes = true },
		"without containers":      func(r *CleanupRequest) { r.RemoveContainers = false },
		"without networks":        func(r *CleanupRequest) { r.RemoveNetworks = false },
		"without takod state":     func(r *CleanupRequest) { r.RemoveTakodState = false },
	}
	if err := validateCleanupRequest(previewTeardownRequest("preview-pr-42")); err != nil {
		t.Fatalf("valid preview teardown rejected: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := previewTeardownRequest("preview-pr-42")
			mutate(&req)
			if err := validateCleanupRequest(req); err == nil {
				t.Fatalf("preview teardown with %s was accepted", name)
			}
		})
	}
}

func TestPreviewTeardownDecodesDataDirOnlyFromServer(t *testing.T) {
	var req CleanupRequest
	if err := json.Unmarshal([]byte(`{"project":"demo","environment":"preview-pr-42","DataDir":"/etc","dataDir":"/etc"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.DataDir != "" {
		t.Fatalf("DataDir decoded from request body: %q", req.DataDir)
	}
}

func TestHandleCleanupPreviewGuardRefusesVolumesWithoutRecord(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(filepath.Join(t.TempDir(), "takod.sock"), dataDir, "test")
	schedule := testBackupScheduleRequest()
	schedule.Environment = "preview-pr-42"
	if _, err := server.backupScheduler.Upsert(context.Background(), schedule); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", runtimeid.VolumeName("demo", "preview-pr-42", "data")+"\n")
	t.Setenv("TAKO_FAKE_PROJECT_PS_OUTPUT", "worker123\n")

	body, _ := json.Marshal(previewTeardownRequest("preview-pr-42"))
	recorder := httptest.NewRecorder()
	server.handleCleanup(recorder, httptest.NewRequest(http.MethodPost, "/v1/cleanup", bytes.NewReader(body)))
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "no preview record") {
		t.Fatalf("body = %q, want guard explanation", recorder.Body.String())
	}
	for _, entry := range readCommandLog(t, logPath) {
		if strings.HasPrefix(entry, "docker ps") || strings.Contains(entry, " rm ") || strings.HasPrefix(entry, "docker stop") {
			t.Fatalf("guard refusal ran teardown command %q", entry)
		}
	}
	if _, err := os.Stat(backupSchedulePath(dataDir, schedule)); err != nil {
		t.Fatalf("guard refusal deleted backup schedule: %v", err)
	}
}

func TestHandleCleanupPreviewWithoutRecordOrVolumesIsNoop(t *testing.T) {
	dataDir := t.TempDir()
	server := NewServer(filepath.Join(t.TempDir(), "takod.sock"), dataDir, "test")
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	body, _ := json.Marshal(previewTeardownRequest("preview-pr-42"))
	recorder := httptest.NewRecorder()
	server.handleCleanup(recorder, httptest.NewRequest(http.MethodPost, "/v1/cleanup", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	var response CleanupResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.PreviewNotFound || response.PreviewRecordRemoved {
		t.Fatalf("response = %+v, want previewNotFound no-op", response)
	}
	for _, entry := range readCommandLog(t, logPath) {
		if !strings.HasPrefix(entry, "docker volume ls") {
			t.Fatalf("no-op teardown ran %q", entry)
		}
	}
}

func TestPreviewTeardownRemovesOnlyPreviewDataInOrderAndRecordLast(t *testing.T) {
	useTempProxyPaths(t)
	dataDir := t.TempDir()
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	env := "preview-pr-42"
	writeTestPreviewRecord(t, dataDir, "demo", env)
	for _, environment := range []string{env, "preview", "preview-pr-420"} {
		writeTestFile(t, filepath.Join(dataDir, "desired", "demo", environment, "revision.json"), "{}")
		writeTestFile(t, filepath.Join(dataDir, "actual", "demo", environment, "containers.json"), "{}")
		writeTestFile(t, filepath.Join(dataDir, "events", "demo", environment+".jsonl"), "{}\n")
		writeTestFile(t, filepath.Join(dataDir, "history", "demo", environment, "history.json"), "{}")
		writeTestFile(t, filepath.Join(serviceFilesRoot, "demo", environment, "web", "config.txt"), "x")
		journal, _ := json.Marshal(volumeOwnershipJournal{SchemaVersion: 1, Project: "demo", Environment: environment, Volume: runtimeid.VolumeName("demo", environment, "data"), Phase: "preexisting"})
		writeTestFile(t, volumeOwnershipJournalPath(filepath.Join(dataDir, volumeOwnershipDirName), runtimeid.VolumeName("demo", environment, "data")), string(journal))
	}
	volume := runtimeid.VolumeName("demo", env, "data")
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", volume+"\n")
	t.Setenv("TAKO_FAKE_PROJECT_PS_OUTPUT", "worker123\n")
	network := runtimeid.NetworkName("demo", env)
	t.Setenv("TAKO_FAKE_NETWORK_LS_OUTPUT", network+"\t<no value>\t<no value>\t<no value>\n"+runtimeid.NetworkName("demo", "preview")+"\t<no value>\t<no value>\t<no value>\n")

	req := previewTeardownRequest(env)
	req.DataDir = dataDir
	response, err := CleanupProject(context.Background(), req)
	if err != nil {
		t.Fatalf("CleanupProject returned error: %v", err)
	}
	if !response.PreviewRecordRemoved || response.VolumesRemoved != 1 {
		t.Fatalf("response = %+v, want record and one volume removed", response)
	}

	entries := readCommandLog(t, logPath)
	index := func(prefix string) int {
		for i, entry := range entries {
			if strings.HasPrefix(entry, prefix) {
				return i
			}
		}
		t.Fatalf("missing %q in %#v", prefix, entries)
		return -1
	}
	containers := index("docker stop worker123")
	networks := index("docker network rm " + network)
	volumes := index("docker volume rm " + volume)
	if !(containers < networks && networks < volumes) {
		t.Fatalf("teardown order containers=%d networks=%d volumes=%d in %#v", containers, networks, volumes, entries)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry, "docker images") || strings.HasPrefix(entry, "docker rmi") || strings.HasPrefix(entry, "docker image") ||
			strings.Contains(entry, "/opt/") || strings.Contains(entry, "prune") ||
			entry == "docker network rm "+runtimeid.NetworkName("demo", "preview") {
			t.Fatalf("preview teardown touched shared resources via %q", entry)
		}
	}

	for _, gone := range []string{
		previewRecordPath(dataDir, "demo", env),
		filepath.Join(dataDir, "desired", "demo", env),
		filepath.Join(dataDir, "actual", "demo", env),
		filepath.Join(dataDir, "events", "demo", env+".jsonl"),
		filepath.Join(dataDir, "history", "demo", env),
		filepath.Join(serviceFilesRoot, "demo", env),
		volumeOwnershipJournalPath(filepath.Join(dataDir, volumeOwnershipDirName), volume),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatalf("%s survived preview teardown (err=%v)", gone, err)
		}
	}
	for _, environment := range []string{"preview", "preview-pr-420"} {
		for _, kept := range []string{
			filepath.Join(dataDir, "desired", "demo", environment, "revision.json"),
			filepath.Join(dataDir, "actual", "demo", environment, "containers.json"),
			filepath.Join(dataDir, "events", "demo", environment+".jsonl"),
			filepath.Join(dataDir, "history", "demo", environment, "history.json"),
			filepath.Join(serviceFilesRoot, "demo", environment, "web", "config.txt"),
			volumeOwnershipJournalPath(filepath.Join(dataDir, volumeOwnershipDirName), runtimeid.VolumeName("demo", environment, "data")),
		} {
			if _, err := os.Stat(kept); err != nil {
				t.Fatalf("preview teardown removed %s of environment %s: %v", kept, environment, err)
			}
		}
	}
}

func TestPreviewTeardownKeepsRecordWhenVolumeRemovalFails(t *testing.T) {
	useTempProxyPaths(t)
	dataDir := t.TempDir()
	oldRoot := serviceFilesRoot
	serviceFilesRoot = t.TempDir()
	defer func() { serviceFilesRoot = oldRoot }()
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()

	env := "preview-pr-42"
	writeTestPreviewRecord(t, dataDir, "demo", env)
	writeTestFile(t, filepath.Join(dataDir, "desired", "demo", env, "revision.json"), "{}")
	volume := runtimeid.VolumeName("demo", env, "data")
	t.Setenv("TAKO_FAKE_VOLUME_LS_OUTPUT", volume+"\n")
	t.Setenv("TAKO_FAKE_FAIL_VOLUME_RM", volume)

	req := previewTeardownRequest(env)
	req.DataDir = dataDir
	if _, err := CleanupProject(context.Background(), req); err == nil {
		t.Fatal("teardown succeeded despite failed volume removal")
	}
	for _, kept := range []string{
		previewRecordPath(dataDir, "demo", env),
		filepath.Join(dataDir, "desired", "demo", env, "revision.json"),
	} {
		if _, err := os.Stat(kept); err != nil {
			t.Fatalf("%s removed after failed teardown step: %v", kept, err)
		}
	}
}

func TestPreviewStateDocumentRules(t *testing.T) {
	dataDir := t.TempDir()
	ctx := context.Background()
	if _, err := WriteStateDocument(ctx, dataDir, StateDocumentRequest{
		Project: "demo", Environment: "production", Document: stateDocumentPreview,
		Content: testPreviewRecordJSON(t, "demo", "production"),
	}); err == nil {
		t.Fatal("preview record accepted for non-preview environment")
	}
	if _, err := WriteStateDocument(ctx, dataDir, StateDocumentRequest{
		Project: "demo", Environment: "preview-pr-42", Document: stateDocumentPreview,
		Content: testPreviewRecordJSON(t, "demo", "preview-pr-43"),
	}); err == nil {
		t.Fatal("preview record with mismatched identity accepted")
	}
	writeTestPreviewRecord(t, dataDir, "demo", "preview-pr-42")
	writeTestPreviewRecord(t, dataDir, "demo", "preview-pr-7")
	writeTestPreviewRecord(t, dataDir, "other", "preview-pr-1")
	writeTestFile(t, filepath.Join(dataDir, "desired", "demo", "production", "revision.json"), "{}")

	if _, err := DeleteStateDocument(ctx, dataDir, StateDocumentRequest{Project: "demo", Environment: "preview-pr-42", Document: stateDocumentPreview}); err == nil {
		t.Fatal("preview record deleted outside ordered teardown")
	}

	list, err := ListPreviewRecords(ctx, dataDir, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var environments []string
	for _, record := range list.Previews {
		environments = append(environments, record.Environment)
	}
	if !slices.Equal(environments, []string{"preview-pr-42", "preview-pr-7"}) {
		t.Fatalf("listed environments = %v", environments)
	}
}

func TestHandleStatePreviewsIsRegisteredReadOnlyRoute(t *testing.T) {
	dataDir := t.TempDir()
	writeTestPreviewRecord(t, dataDir, "demo", "preview-pr-42")
	server := &Server{dataDir: dataDir}
	found := false
	for _, route := range server.registeredRoutes() {
		if route.path == "/v1/state/previews" {
			found = true
		}
	}
	if !found {
		t.Fatal("/v1/state/previews is not registered")
	}
	recorder := httptest.NewRecorder()
	server.handleStatePreviews(recorder, httptest.NewRequest(http.MethodGet, "/v1/state/previews?project=demo", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response PreviewListResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Previews) != 1 || response.Previews[0].Name != "pr-42" {
		t.Fatalf("response = %+v", response)
	}
	recorder = httptest.NewRecorder()
	server.handleStatePreviews(recorder, httptest.NewRequest(http.MethodPost, "/v1/state/previews?project=demo", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", recorder.Code)
	}
}
