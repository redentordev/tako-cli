package takod

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func intPtr(value int) *int { return &value }

func TestValidateLogsStreamRequest(t *testing.T) {
	valid := func() LogsStreamRequest {
		return LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}}
	}
	tests := []struct {
		name    string
		mutate  func(*LogsStreamRequest)
		wantErr string
	}{
		{name: "valid", mutate: func(*LogsStreamRequest) {}},
		{name: "jobs only", mutate: func(r *LogsStreamRequest) { r.Services = nil; r.IncludeJobs = []string{"nightly"}; r.Follow = true }},
		{name: "missing project", mutate: func(r *LogsStreamRequest) { r.Project = "" }, wantErr: "project is required"},
		{name: "unsafe project", mutate: func(r *LogsStreamRequest) { r.Project = "../x" }, wantErr: "invalid project name"},
		{name: "unsafe environment", mutate: func(r *LogsStreamRequest) { r.Environment = "prod;rm" }, wantErr: "invalid environment name"},
		{name: "no services", mutate: func(r *LogsStreamRequest) { r.Services = nil }, wantErr: "at least one service"},
		{name: "unsafe service", mutate: func(r *LogsStreamRequest) { r.Services = []string{"Web!"} }, wantErr: "invalid service name"},
		{name: "unsafe job", mutate: func(r *LogsStreamRequest) { r.IncludeJobs = []string{"--all"} }, wantErr: "invalid service name"},
		{name: "negative tail", mutate: func(r *LogsStreamRequest) { r.Tail = intPtr(-1) }, wantErr: "tail cannot be negative"},
		{name: "tail too large", mutate: func(r *LogsStreamRequest) { r.Tail = intPtr(maxLogTail + 1) }, wantErr: "tail cannot exceed"},
		{name: "max tail", mutate: func(r *LogsStreamRequest) { r.Tail = intPtr(maxLogTail) }},
		{name: "grep too long", mutate: func(r *LogsStreamRequest) { r.Grep = strings.Repeat("a", MaxLogsGrepBytes+1) }, wantErr: "exceeds 1024 bytes"},
		{name: "grep invalid", mutate: func(r *LogsStreamRequest) { r.Grep = "(" }, wantErr: "invalid grep pattern"},
		{name: "since duration", mutate: func(r *LogsStreamRequest) { r.Since = "15m" }},
		{name: "since rfc3339", mutate: func(r *LogsStreamRequest) { r.Since = "2026-10-01T00:00:00Z" }},
		{name: "since unix", mutate: func(r *LogsStreamRequest) { r.Since = "1790000000" }},
		{name: "since bad", mutate: func(r *LogsStreamRequest) { r.Since = "yesterday" }, wantErr: "invalid since value"},
		{name: "since negative", mutate: func(r *LogsStreamRequest) { r.Since = "-5m" }, wantErr: "duration must be positive"},
		{name: "until with follow", mutate: func(r *LogsStreamRequest) { r.Until = "5m"; r.Follow = true }, wantErr: "until cannot be combined with follow"},
		{name: "until before since", mutate: func(r *LogsStreamRequest) {
			r.Since = "2026-10-02T00:00:00Z"
			r.Until = "2026-10-01T00:00:00Z"
		}, wantErr: "until must not be earlier"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := valid()
			tt.mutate(&req)
			_, err := validateLogsStreamRequest(&req)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestNormalizeLogsTimeValue(t *testing.T) {
	if got, abs, err := NormalizeLogsTimeValue("since", "1h30m"); err != nil || got != "1h30m" || abs != nil {
		t.Fatalf("duration = %q %v %v", got, abs, err)
	}
	if got, abs, err := NormalizeLogsTimeValue("since", "2026-10-01T02:00:00+02:00"); err != nil || got != "2026-10-01T00:00:00Z" || abs == nil {
		t.Fatalf("rfc3339 = %q %v %v", got, abs, err)
	}
	if got, _, err := NormalizeLogsTimeValue("since", "0"); err != nil || got != "1970-01-01T00:00:00Z" {
		t.Fatalf("unix = %q %v", got, err)
	}
	if _, _, err := NormalizeLogsTimeValue("since", "yesterday"); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Fatalf("expected accepted formats in error, got %v", err)
	}
}

func TestListLogsContainersUsesProjectFiltersAndParsesLabels(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	restore := useFakeCommands(t, logPath)
	defer restore()
	t.Setenv("TAKO_FAKE_PROJECT_PS_OUTPUT", strings.Join([]string{
		"aaa|demo_production_web_1|web|1|rev1|",
		"bbb|demo_production_web_2|web|2|rev1|",
		"ccc|demo_production_worker_1|worker|1|rev1|",
		"ddd|job-nightly-1|nightly|||job",
		"eee|exec-web|web|||exec",
		"malformed",
	}, "\n")+"\n")

	streamer := newLogsStreamer(LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}, IncludeJobs: []string{"nightly"}}, nil, io.Discard)
	sources, err := streamer.discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, source := range sources {
		names = append(names, source.Service+"."+source.Slot+"="+source.ID)
	}
	want := []string{"nightly.run=ddd", "web.1=aaa", "web.2=bbb"}
	if !slices.Equal(names, want) {
		t.Fatalf("sources = %v, want %v", names, want)
	}
	entries := readCommandLog(t, logPath)
	if len(entries) != 1 || !strings.Contains(entries[0], "ps -a --filter label=tako.project=demo --filter label=tako.environment=production") {
		t.Fatalf("docker commands = %#v", entries)
	}
}

func TestListLogsContainersZeroContainersIsNotAnError(t *testing.T) {
	restore := useFakeCommands(t, filepath.Join(t.TempDir(), "commands.log"))
	defer restore()
	streamer := newLogsStreamer(LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}}, nil, io.Discard)
	sources, err := streamer.discover(context.Background())
	if err != nil || len(sources) != 0 {
		t.Fatalf("sources = %#v, err = %v", sources, err)
	}
}

// fakeLogsDocker scripts container discovery, `docker logs`, and `docker
// events` for stream tests without a Docker daemon.
type fakeLogsDocker struct {
	mu     sync.Mutex
	rows   []logsContainerRow
	stdout map[string][]string
	stderr map[string][]string
	fail   map[string]string
	calls  [][]string
	stops  map[string]chan struct{}
	events chan logsContainerEvent
}

func newFakeLogsDocker() *fakeLogsDocker {
	return &fakeLogsDocker{
		stdout: map[string][]string{},
		stderr: map[string][]string{},
		fail:   map[string]string{},
		stops:  map[string]chan struct{}{},
		events: make(chan logsContainerEvent, 16),
	}
}

func (f *fakeLogsDocker) install(t *testing.T) {
	t.Helper()
	oldList, oldRun, oldWatch := logsListContainers, logsRunDockerLogs, logsWatchContainers
	t.Cleanup(func() { logsListContainers, logsRunDockerLogs, logsWatchContainers = oldList, oldRun, oldWatch })
	logsListContainers = func(context.Context, string, string) ([]logsContainerRow, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return append([]logsContainerRow(nil), f.rows...), nil
	}
	logsRunDockerLogs = f.run
	logsWatchContainers = func(ctx context.Context, _ string, _ string, handle func(logsContainerEvent)) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case event := <-f.events:
				handle(event)
			}
		}
	}
}

func (f *fakeLogsDocker) stop(id string) chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stops[id] == nil {
		f.stops[id] = make(chan struct{})
	}
	return f.stops[id]
}

func (f *fakeLogsDocker) run(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	id := args[len(args)-1]
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	outLines := append([]string(nil), f.stdout[id]...)
	errLines := append([]string(nil), f.stderr[id]...)
	failure := f.fail[id]
	f.mu.Unlock()
	if failure != "" {
		_, _ = io.WriteString(stderr, failure+"\n")
		return fmt.Errorf("exit status 1")
	}
	for _, line := range outLines {
		_, _ = io.WriteString(stdout, line+"\n")
	}
	for _, line := range errLines {
		_, _ = io.WriteString(stderr, line+"\n")
	}
	if slices.Contains(args, "-f") {
		select {
		case <-ctx.Done():
		case <-f.stop(id):
		}
	}
	return nil
}

func decodeTestFrames(t *testing.T, data string) []LogsStreamFrame {
	t.Helper()
	var frames []LogsStreamFrame
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		var frame LogsStreamFrame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("invalid frame %q: %v", scanner.Text(), err)
		}
		frames = append(frames, frame)
	}
	return frames
}

func lineData(frames []LogsStreamFrame) []string {
	var out []string
	for _, frame := range frames {
		if frame.Type == LogsFrameLine {
			out = append(out, frame.Service+"."+frame.Slot+"/"+frame.Stream+":"+frame.Data)
		}
	}
	return out
}

func TestStreamServiceLogFramesOrdersLinesWithinContainerAndTagsStreams(t *testing.T) {
	fake := newFakeLogsDocker()
	fake.install(t)
	fake.rows = []logsContainerRow{{ID: "web1", Name: "demo_production_web_1", Service: "web", Slot: "1", Revision: "rev1"}}
	fake.stdout["web1"] = []string{
		"2026-10-11T12:00:01.000000000Z first",
		"2026-10-11T12:00:03.000000000Z third",
	}
	fake.stderr["web1"] = []string{"2026-10-11T12:00:02.000000000Z second"}

	var buffer bytes.Buffer
	if err := StreamServiceLogFrames(context.Background(), LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}}, &buffer); err != nil {
		t.Fatal(err)
	}
	frames := decodeTestFrames(t, buffer.String())
	got := lineData(frames)
	want := []string{"web.1/stdout:first", "web.1/stderr:second", "web.1/stdout:third"}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for _, frame := range frames {
		if frame.Type == LogsFrameLine && (frame.Container != "demo_production_web_1" || frame.Revision != "rev1" || frame.Time == "") {
			t.Fatalf("line frame missing attribution: %#v", frame)
		}
	}
	if frames[0].Type != LogsFrameReady || frames[0].Sources == nil || *frames[0].Sources != 1 {
		t.Fatalf("first frame = %#v, want ready with one source", frames[0])
	}
	if args := fake.calls[0]; !slices.Equal(args, []string{"logs", "-t", "--tail", "100", "web1"}) {
		t.Fatalf("docker logs args = %v", args)
	}
}

func TestStreamServiceLogFramesTailCountsLastMatches(t *testing.T) {
	fake := newFakeLogsDocker()
	fake.install(t)
	fake.rows = []logsContainerRow{{ID: "web1", Name: "web-1", Service: "web", Slot: "1"}}
	for i := 0; i < 50; i++ {
		message := fmt.Sprintf("request %d ok", i)
		if i%5 == 0 {
			message = fmt.Sprintf("request %d TIMEOUT", i)
		}
		fake.stdout["web1"] = append(fake.stdout["web1"], fmt.Sprintf("2026-10-11T12:00:%02d.000000000Z %s", i, message))
	}
	var buffer bytes.Buffer
	err := StreamServiceLogFrames(context.Background(), LogsStreamRequest{
		Project: "demo", Environment: "production", Services: []string{"web"},
		Tail: intPtr(3), Grep: "timeout", IgnoreCase: true, Since: "1h",
	}, &buffer)
	if err != nil {
		t.Fatal(err)
	}
	got := lineData(decodeTestFrames(t, buffer.String()))
	want := []string{"web.1/stdout:request 35 TIMEOUT", "web.1/stdout:request 40 TIMEOUT", "web.1/stdout:request 45 TIMEOUT"}
	if !slices.Equal(got, want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	if args := fake.calls[0]; !slices.Equal(args, []string{"logs", "-t", "--tail", "10000", "--since", "1h", "web1"}) {
		t.Fatalf("grep scan args = %v", args)
	}
}

func TestStreamServiceLogFramesIgnoresDockerDiagnosticsAndVanishedContainers(t *testing.T) {
	fake := newFakeLogsDocker()
	fake.install(t)
	fake.rows = []logsContainerRow{{ID: "gone", Name: "gone-1", Service: "web", Slot: "1"}, {ID: "web2", Name: "web-2", Service: "web", Slot: "2"}}
	fake.fail["gone"] = "Error response from daemon: No such container: gone"
	fake.stdout["web2"] = []string{"2026-10-11T12:00:01Z hello"}
	var buffer bytes.Buffer
	if err := StreamServiceLogFrames(context.Background(), LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}}, &buffer); err != nil {
		t.Fatal(err)
	}
	if got := lineData(decodeTestFrames(t, buffer.String())); !slices.Equal(got, []string{"web.2/stdout:hello"}) {
		t.Fatalf("lines = %v", got)
	}

	fake.fail["web2"] = "permission denied"
	buffer.Reset()
	err := StreamServiceLogFrames(context.Background(), LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}}, &buffer)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected docker failure, got %v", err)
	}
}

// syncBuffer is a goroutine-safe frame sink for follow tests.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForFrames(t *testing.T, buffer *syncBuffer, ok func([]LogsStreamFrame) bool) []LogsStreamFrame {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		frames := decodeTestFrames(t, buffer.String())
		if ok(frames) {
			return frames
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for frames; got:\n%s", buffer.String())
	return nil
}

func hasFrame(frames []LogsStreamFrame, match func(LogsStreamFrame) bool) bool {
	return slices.ContainsFunc(frames, match)
}

func TestStreamServiceLogFramesFollowAttachesReplacementContainers(t *testing.T) {
	fake := newFakeLogsDocker()
	fake.install(t)
	fake.rows = []logsContainerRow{{ID: "old", Name: "web-old", Service: "web", Slot: "1", Revision: "rev1"}}
	fake.stdout["old"] = []string{"2026-10-11T12:00:01Z from old"}
	fake.stdout["new"] = []string{"2026-10-11T12:01:01Z from new"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffer := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- StreamServiceLogFrames(ctx, LogsStreamRequest{Project: "demo", Environment: "production", Services: []string{"web"}, Follow: true}, buffer)
	}()
	waitForFrames(t, buffer, func(frames []LogsStreamFrame) bool {
		return slices.Contains(lineData(frames), "web.1/stdout:from old")
	})

	// A deploy starts the replacement and stops the old container.
	start := time.Date(2026, 10, 11, 12, 1, 0, 0, time.UTC)
	fake.events <- logsContainerEvent{Action: "start", ID: "new", Time: start, Attributes: map[string]string{"name": "web-new", "tako.service": "web", "tako.slot": "1", "tako.revision": "rev2"}}
	fake.events <- logsContainerEvent{Action: "start", ID: "other", Time: start, Attributes: map[string]string{"name": "api-1", "tako.service": "api", "tako.slot": "1"}}
	close(fake.stop("old"))
	frames := waitForFrames(t, buffer, func(frames []LogsStreamFrame) bool {
		return slices.Contains(lineData(frames), "web.1/stdout:from new") && hasFrame(frames, func(f LogsStreamFrame) bool {
			return f.Type == LogsFrameSource && f.Event == LogsSourceDetached && f.Container == "web-old"
		})
	})
	if hasFrame(frames, func(f LogsStreamFrame) bool { return f.Service == "api" }) {
		t.Fatalf("unselected service attached: %s", buffer.String())
	}
	if hasFrame(frames, func(f LogsStreamFrame) bool { return f.Type == LogsFrameEnd }) {
		t.Fatal("follow stream must not end while running")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("follow returned %v", err)
	}
	var newArgs []string
	fake.mu.Lock()
	for _, call := range fake.calls {
		if call[len(call)-1] == "new" {
			newArgs = call
		}
	}
	fake.mu.Unlock()
	if !slices.Equal(newArgs, []string{"logs", "-t", "-f", "--since", "2026-10-11T12:01:00Z", "new"}) {
		t.Fatalf("replacement attach args = %v", newArgs)
	}
}

func TestStreamServiceLogFramesFollowJobRuns(t *testing.T) {
	fake := newFakeLogsDocker()
	fake.install(t)
	fake.stdout["run1"] = []string{"2026-10-11T12:00:01Z run one"}
	fake.stdout["run2"] = []string{"2026-10-11T13:00:01Z run two"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	buffer := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- StreamServiceLogFrames(ctx, LogsStreamRequest{Project: "demo", Environment: "production", IncludeJobs: []string{"nightly"}, Follow: true}, buffer)
	}()
	waitForFrames(t, buffer, func(frames []LogsStreamFrame) bool {
		return hasFrame(frames, func(f LogsStreamFrame) bool { return f.Type == LogsFrameReady && f.Sources != nil && *f.Sources == 0 })
	})
	jobAttrs := func(name string) map[string]string {
		return map[string]string{"name": name, "tako.service": "nightly", "tako.role": "job"}
	}
	for _, run := range []string{"run1", "run2"} {
		fake.events <- logsContainerEvent{Action: "start", ID: run, Time: time.Now(), Attributes: jobAttrs(run)}
		waitForFrames(t, buffer, func(frames []LogsStreamFrame) bool {
			return hasFrame(frames, func(f LogsStreamFrame) bool { return f.Type == LogsFrameLine && f.Container == run })
		})
		attrs := jobAttrs(run)
		attrs["exitCode"] = "3"
		fake.events <- logsContainerEvent{Action: "die", ID: run, Time: time.Now(), Attributes: attrs}
		close(fake.stop(run))
	}
	frames := waitForFrames(t, buffer, func(frames []LogsStreamFrame) bool {
		count := 0
		for _, f := range frames {
			if f.Type == LogsFrameRun && f.Event == LogsRunFinished {
				count++
			}
		}
		return count == 2
	})
	cancel()
	<-done
	var runEvents []string
	for _, f := range frames {
		if f.Type == LogsFrameRun {
			code := ""
			if f.ExitCode != nil {
				code = fmt.Sprintf("(%d)", *f.ExitCode)
			}
			runEvents = append(runEvents, f.Container+":"+f.Event+code)
		}
	}
	want := []string{"run1:started", "run1:finished(3)", "run2:started", "run2:finished(3)"}
	if !slices.Equal(runEvents, want) {
		t.Fatalf("run frames = %v, want %v", runEvents, want)
	}
	if got := lineData(frames); !slices.Equal(got, []string{"nightly.run/stdout:run one", "nightly.run/stdout:run two"}) {
		t.Fatalf("job lines = %v", got)
	}
}

func TestHandleLogsStreamRequiresPost(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	recorder := httptest.NewRecorder()
	server.handleLogsStream(recorder, httptest.NewRequest(http.MethodGet, "/v1/logs/stream", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", recorder.Code)
	}
}

func TestHandleLogsStreamRejectsInvalidRequests(t *testing.T) {
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	for _, body := range []string{
		`not json`,
		`{"project":"demo","environment":"production"}`,
		`{"project":"demo","environment":"production","services":["web"],"grep":"("}`,
		`{"project":"demo","environment":"production","services":["web"],"until":"5m","follow":true}`,
	} {
		recorder := httptest.NewRecorder()
		server.handleLogsStream(recorder, httptest.NewRequest(http.MethodPost, "/v1/logs/stream", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400 (%s)", body, recorder.Code, recorder.Body.String())
		}
	}
}

func TestHandleLogsStreamFramesSuccessAndMidStreamErrors(t *testing.T) {
	fake := newFakeLogsDocker()
	fake.install(t)
	fake.rows = []logsContainerRow{{ID: "web1", Name: "web-1", Service: "web", Slot: "1"}, {ID: "web2", Name: "web-2", Service: "web", Slot: "2"}}
	fake.stdout["web1"] = []string{"2026-10-11T12:00:01Z hello"}
	fake.stdout["web2"] = []string{"2026-10-11T12:00:02Z world"}
	server := NewServer("/tmp/takod-test.sock", t.TempDir(), "test")
	body := `{"project":"demo","environment":"production","services":["web"]}`

	recorder := httptest.NewRecorder()
	server.handleLogsStream(recorder, httptest.NewRequest(http.MethodPost, "/v1/logs/stream", strings.NewReader(body)))
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("status = %d content-type = %q", recorder.Code, recorder.Header().Get("Content-Type"))
	}
	frames := decodeTestFrames(t, recorder.Body.String())
	if last := frames[len(frames)-1]; last.Type != LogsFrameEnd {
		t.Fatalf("last frame = %#v, want end", last)
	}

	fake.fail["web2"] = "daemon exploded"
	recorder = httptest.NewRecorder()
	server.handleLogsStream(recorder, httptest.NewRequest(http.MethodPost, "/v1/logs/stream", strings.NewReader(body)))
	frames = decodeTestFrames(t, recorder.Body.String())
	last := frames[len(frames)-1]
	if recorder.Code != http.StatusOK || last.Type != LogsFrameError || !strings.Contains(last.Message, "daemon exploded") {
		t.Fatalf("mid-stream failure: status=%d frames=%s", recorder.Code, recorder.Body.String())
	}
	if !slices.Contains(lineData(frames), "web.1/stdout:hello") {
		t.Fatalf("lines before the failure were lost: %s", recorder.Body.String())
	}
}

func TestParseDockerContainerEvent(t *testing.T) {
	event, ok := parseDockerContainerEvent([]byte(`{"status":"die","id":"abc","Type":"container","Action":"die","Actor":{"ID":"abc","Attributes":{"exitCode":"2","tako.service":"nightly"}},"time":1790000000,"timeNano":1790000000123456789}`))
	if !ok || event.Action != "die" || event.ID != "abc" || event.Attributes["exitCode"] != "2" || event.Time.UnixNano() != 1790000000123456789 {
		t.Fatalf("event = %#v ok=%v", event, ok)
	}
	if _, ok := parseDockerContainerEvent([]byte(`garbage`)); ok {
		t.Fatal("garbage parsed as event")
	}
}
