package takod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
}

type stalledUploadBody struct {
	entered  chan struct{}
	timedOut chan struct{}
	once     sync.Once
}

func (b *stalledUploadBody) Read([]byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.timedOut
	return 0, os.ErrDeadlineExceeded
}

func (*stalledUploadBody) Close() error { return nil }

type timedDeadlineRecorder struct {
	*httptest.ResponseRecorder
	body *stalledUploadBody
	once sync.Once
}

func (r *timedDeadlineRecorder) SetReadDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		time.AfterFunc(time.Until(deadline), func() { r.once.Do(func() { close(r.body.timedOut) }) })
	}
	return nil
}

func (r *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	r.deadlines = append(r.deadlines, deadline)
	return nil
}

func TestRequireFreeDiskAllowsDisabledAdmission(t *testing.T) {
	called := false
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{
		DiskAvailable: func(string) (int64, error) {
			called = true
			return 0, errors.New("must not be called")
		},
	})
	recorder := httptest.NewRecorder()
	if !server.requireFreeDisk(recorder) {
		t.Fatal("disabled disk admission rejected the operation")
	}
	if called {
		t.Fatal("disabled disk admission probed the filesystem")
	}
}

func TestRequireFreeDiskChecksEveryAffectedFilesystem(t *testing.T) {
	var paths []string
	server := NewServerWithOptions("/tmp/takod-test.sock", "/state", "test", ServerOptions{
		MinimumFreeDiskBytes: 100,
		DiskIdentity:         func(path string) (string, error) { return path, nil },
		DiskAvailable: func(path string) (int64, error) {
			paths = append(paths, path)
			if path == "/docker" {
				return 99, nil
			}
			return 101, nil
		},
	})
	recorder := httptest.NewRecorder()
	if server.requireFreeDisk(recorder, "/state", "/docker") {
		t.Fatal("low Docker filesystem was ignored")
	}
	if recorder.Code != http.StatusInsufficientStorage || len(paths) != 2 || paths[0] != "/state" || paths[1] != "/docker" {
		t.Fatalf("admission paths/response = %v, %d %q", paths, recorder.Code, recorder.Body.String())
	}
}

func TestProxyFileAdmissionChecksRouteAndDynamicFilesystems(t *testing.T) {
	var paths []string
	server := NewServerWithOptions("/tmp/takod-test.sock", "/state", "test", ServerOptions{
		MinimumFreeDiskBytes: 100,
		DiskIdentity:         func(path string) (string, error) { return path, nil },
		DiskAvailable: func(path string) (int64, error) {
			paths = append(paths, path)
			if path == proxyDynamicDir {
				return 99, nil
			}
			return 101, nil
		},
	})
	body, _ := json.Marshal(ProxyFileRequest{Name: "demo.json", Content: `{}`})
	recorder := httptest.NewRecorder()
	server.handleProxyFile(recorder, httptest.NewRequest(http.MethodPut, "/v1/proxy-file", bytes.NewReader(body)))
	if recorder.Code != http.StatusInsufficientStorage || len(paths) != 2 || paths[0] != proxyRoutesDir || paths[1] != proxyDynamicDir {
		t.Fatalf("proxy admission paths/response = %v, %d %q", paths, recorder.Code, recorder.Body.String())
	}
}

func TestReconcileDiskAdmissionAllowsScaleToZeroButBlocksGrowth(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	probes := 0
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{
		MinimumFreeDiskBytes: 100,
		DockerDataRoot:       "/docker",
		DiskAvailable: func(string) (int64, error) {
			probes++
			return 99, nil
		},
	})
	base := ReconcileServiceRequest{
		Project: "demo", Environment: "production", Service: "web",
		Image: "registry.example.com/demo/web:latest", Network: "tako_demo_production",
	}
	body, _ := json.Marshal(base)
	recorder := httptest.NewRecorder()
	server.handleReconcileService(recorder, httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || probes != 0 {
		t.Fatalf("scale-to-zero response = %d %q, probes=%d", recorder.Code, recorder.Body.String(), probes)
	}
	base.Containers = []ContainerSpec{{Name: "tako-demo-production-web-1"}}
	body, _ = json.Marshal(base)
	recorder = httptest.NewRecorder()
	server.handleReconcileService(recorder, httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", bytes.NewReader(body)))
	if recorder.Code != http.StatusInsufficientStorage || probes != 1 {
		t.Fatalf("growing reconcile response = %d %q, probes=%d", recorder.Code, recorder.Body.String(), probes)
	}
	base.Containers = nil
	base.FileSetID = strings.Repeat("a", 64)
	base.Files = []ServiceFileBundle{{Name: "config", Target: "/app/config", Entries: []ServiceFileEntry{{Data: []byte("x"), Mode: 0600}}}}
	body, _ = json.Marshal(base)
	recorder = httptest.NewRecorder()
	server.handleReconcileService(recorder, httptest.NewRequest(http.MethodPost, "/v1/reconcile-service", bytes.NewReader(body)))
	if recorder.Code != http.StatusInsufficientStorage || probes != 2 {
		t.Fatalf("file-publishing scale-to-zero response = %d %q, probes=%d", recorder.Code, recorder.Body.String(), probes)
	}
}

func TestPortAllocationNormalizesOmittedKindBeforeAdmission(t *testing.T) {
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{
		MinimumFreeDiskBytes: 100,
		DiskAvailable:        func(string) (int64, error) { return 99, nil },
	})
	request := PortAllocationRequest{Project: "demo", Environment: "production", Service: "web", Slot: 1, HostIP: "127.0.0.1", ContainerPort: 8080, PreferredPort: 20000, MinPort: 20000, MaxPort: 21000}
	body, _ := json.Marshal(request)
	recorder := httptest.NewRecorder()
	server.handlePortAllocate(recorder, httptest.NewRequest(http.MethodPost, "/v1/ports/allocate", bytes.NewReader(body)))
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("omitted port kind was not normalized before admission: %d %q", recorder.Code, recorder.Body.String())
	}
}

func TestUploadReservationPreservesPlatformFloor(t *testing.T) {
	server := NewServerWithOptions("/tmp/takod-test.sock", "/state", "test", ServerOptions{
		MinimumFreeDiskBytes: 100,
		DiskAvailable:        func(string) (int64, error) { return 150, nil },
	})
	if err := server.checkFreeDisk(50, "/docker"); err != nil {
		t.Fatalf("exactly preserved floor rejected: %v", err)
	}
	if err := server.checkFreeDisk(51, "/docker"); err == nil || !strings.Contains(err.Error(), "cannot preserve") {
		t.Fatalf("floor-consuming upload accepted: %v", err)
	}
	if got := uploadReservation(-1, 2048); got != 2048 {
		t.Fatalf("unknown upload reservation = %d", got)
	}
}

func TestConcurrentUploadReservationsCannotOvercommitFloor(t *testing.T) {
	server := NewServerWithOptions("/tmp/takod-test.sock", "/state", "test", ServerOptions{
		MinimumFreeDiskBytes: 100,
		DiskAvailable:        func(string) (int64, error) { return 200, nil },
		DiskIdentity:         func(string) (string, error) { return "shared-device", nil },
	})
	first := httptest.NewRecorder()
	release, ok := server.reserveFreeDisk(first, 60, "/docker")
	if !ok {
		t.Fatalf("first reservation denied: %d %q", first.Code, first.Body.String())
	}
	ordinary := httptest.NewRecorder()
	if server.requireFreeDisk(ordinary, "/alias/on-same-device") {
		t.Fatal("ordinary growth bypassed an active reservation through an alias path")
	}
	second := httptest.NewRecorder()
	if secondRelease, secondOK := server.reserveFreeDisk(second, 60, "/docker"); secondOK {
		secondRelease()
		t.Fatal("concurrent reservation overcommitted the platform floor")
	}
	if second.Code != http.StatusInsufficientStorage {
		t.Fatalf("second reservation response = %d %q", second.Code, second.Body.String())
	}
	release()
	third := httptest.NewRecorder()
	if thirdRelease, thirdOK := server.reserveFreeDisk(third, 60, "/docker"); !thirdOK {
		t.Fatalf("released reservation remained charged: %d %q", third.Code, third.Body.String())
	} else {
		thirdRelease()
	}
}

func TestBackgroundSchedulersUseResourceAdmission(t *testing.T) {
	backup := NewBackupScheduler(t.TempDir())
	backupCalls := 0
	backup.admit = func(...string) error { backupCalls++; return errors.New("low disk") }
	backup.runScheduledBackup(BackupScheduleRequest{Project: "demo", Environment: "production", Service: "web", Volumes: []BackupScheduleVolume{{Volume: "data", DockerVolume: "demo-data"}, {Volume: "uploads", DockerVolume: "demo-uploads"}}})
	if backupCalls != 2 {
		t.Fatalf("scheduled backup admission calls = %d", backupCalls)
	}

	jobs := NewJobScheduler(t.TempDir())
	jobs.admit = func(...string) error { return errors.New("low disk") }
	ran := false
	jobs.runJob = func(context.Context, JobSpec, string, io.Writer) (int, error) { ran = true; return 0, nil }
	spec := JobSpec{Project: "demo", Environment: "production", Name: "worker"}
	record := jobs.executeReservedJob(context.Background(), spec, JobTriggerSchedule, nil)
	if ran || record.Status != JobRunStatusFailed || !strings.Contains(record.Output, "resource admission") {
		t.Fatalf("scheduled job bypassed admission: ran=%v record=%#v", ran, record)
	}

	certificates := NewCertificateScheduler(t.TempDir())
	certificates.admit = func(...string) error { return errors.New("low disk") }
	if err := certificates.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "resource admission") {
		t.Fatalf("certificate scheduler bypassed admission: %v", err)
	}
}

func TestRequireFreeDiskFailsClosed(t *testing.T) {
	tests := []struct {
		name      string
		available int64
		err       error
		wantCode  int
		wantBody  string
	}{
		{name: "below floor", available: 99, wantCode: http.StatusInsufficientStorage, wantBody: "below 100-byte platform minimum"},
		{name: "probe failure", err: errors.New("statfs failed"), wantCode: http.StatusServiceUnavailable, wantBody: "failed to verify free disk"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{
				MinimumFreeDiskBytes: 100,
				DiskAvailable:        func(string) (int64, error) { return test.available, test.err },
			})
			recorder := httptest.NewRecorder()
			if server.requireFreeDisk(recorder) {
				t.Fatal("disk admission unexpectedly allowed the operation")
			}
			if recorder.Code != test.wantCode || !strings.Contains(recorder.Body.String(), test.wantBody) {
				t.Fatalf("response = %d %q, want %d containing %q", recorder.Code, recorder.Body.String(), test.wantCode, test.wantBody)
			}
		})
	}
}

func TestAcquireBuildSlotEnforcesAndReleasesLimit(t *testing.T) {
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{MaximumConcurrentBuilds: 1})
	release, ok := server.acquireBuildSlot()
	if !ok {
		t.Fatal("first build was rejected")
	}
	if secondRelease, secondOK := server.acquireBuildSlot(); secondOK || secondRelease != nil {
		t.Fatal("second concurrent build was accepted")
	}
	release()
	release()
	thirdRelease, thirdOK := server.acquireBuildSlot()
	if !thirdOK {
		t.Fatal("build slot was not released")
	}
	thirdRelease()
}

func TestAvailableDiskBytesUsesExistingAncestor(t *testing.T) {
	available, err := availableDiskBytes(filepath.Join(t.TempDir(), "not", "created", "yet"))
	if err != nil || available <= 0 {
		t.Fatalf("available bytes = %d, %v", available, err)
	}
}

func TestUploadReadDeadlineIsInstalledAndCleared(t *testing.T) {
	recorder := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	clear := setUploadReadDeadline(recorder, time.Minute)
	if len(recorder.deadlines) != 1 || recorder.deadlines[0].IsZero() {
		t.Fatalf("upload deadline was not installed: %v", recorder.deadlines)
	}
	clear()
	if len(recorder.deadlines) != 2 || !recorder.deadlines[1].IsZero() {
		t.Fatalf("upload deadline was not cleared: %v", recorder.deadlines)
	}
}

func TestStalledBuildUploadTimesOutAndReleasesSlot(t *testing.T) {
	body := &stalledUploadBody{entered: make(chan struct{}), timedOut: make(chan struct{})}
	recorder := &timedDeadlineRecorder{ResponseRecorder: httptest.NewRecorder(), body: body}
	server := NewServerWithOptions("/tmp/takod-test.sock", t.TempDir(), "test", ServerOptions{
		MaximumConcurrentBuilds: 1,
		UploadReadTimeout:       20 * time.Millisecond,
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/build?image=demo/web:test", nil)
	request.Body = body
	request.ContentLength = -1
	done := make(chan struct{})
	go func() {
		server.handleImageBuild(recorder, request)
		close(done)
	}()
	select {
	case <-body.entered:
	case <-time.After(time.Second):
		t.Fatal("build handler did not begin reading the upload")
	}
	if release, ok := server.acquireBuildSlot(); ok {
		release()
		t.Fatal("stalled build did not hold its concurrency slot")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled build did not time out")
	}
	if release, ok := server.acquireBuildSlot(); !ok {
		t.Fatal("timed-out build did not release its concurrency slot")
	} else {
		release()
	}
}
