package takod

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogsStreamRequest is the body of POST /v1/logs/stream (capability
// logs.stream-v1). One request streams every selected service (and, for
// follow, named jobs) on this node as NDJSON frames.
type LogsStreamRequest struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Services    []string `json:"services,omitempty"`
	// IncludeJobs names kind: job services whose one-off run containers
	// (tako.role=job) are streamed too.
	IncludeJobs []string `json:"includeJobs,omitempty"`
	// Tail limits historical lines per container; nil means 100. With Grep
	// it counts matching lines.
	Tail       *int   `json:"tail,omitempty"`
	Since      string `json:"since,omitempty"`
	Until      string `json:"until,omitempty"`
	Grep       string `json:"grep,omitempty"`
	IgnoreCase bool   `json:"ignoreCase,omitempty"`
	Follow     bool   `json:"follow,omitempty"`
}

// Logs stream frame types written by POST /v1/logs/stream.
const (
	LogsFrameLine   = "line"
	LogsFrameSource = "source"
	LogsFrameRun    = "run"
	LogsFrameReady  = "ready"
	LogsFrameEnd    = "end"
	LogsFrameError  = "error"

	LogsSourceAttached = "attached"
	LogsSourceDetached = "detached"
	LogsRunStarted     = "started"
	LogsRunFinished    = "finished"

	// LogsJobSlot is the replica attribution used for job run containers.
	LogsJobSlot = "run"

	// MaxLogsGrepBytes bounds the --grep pattern size.
	MaxLogsGrepBytes = 1024
	defaultLogTail   = 100
)

// LogsStreamFrame is one NDJSON frame of the logs stream. Unknown frame
// types and fields must be ignored by clients.
type LogsStreamFrame struct {
	Type      string `json:"type"`
	Event     string `json:"event,omitempty"`
	Service   string `json:"service,omitempty"`
	Slot      string `json:"slot,omitempty"`
	Container string `json:"container,omitempty"`
	Revision  string `json:"revision,omitempty"`
	Stream    string `json:"stream,omitempty"`
	Time      string `json:"time,omitempty"`
	Data      string `json:"data,omitempty"`
	Job       string `json:"job,omitempty"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	Sources   *int   `json:"sources,omitempty"`
	Message   string `json:"message,omitempty"`
}

// NormalizeLogsTimeValue validates a --since/--until value: a positive Go
// duration (kept verbatim so Docker evaluates it against the node clock),
// an RFC3339 timestamp, or Unix seconds (both normalized to RFC3339Nano
// UTC). The absolute time is returned for absolute values.
func NormalizeLogsTimeValue(field string, value string) (string, *time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil, nil
	}
	if duration, err := time.ParseDuration(value); err == nil && !unixPattern.MatchString(value) {
		if duration <= 0 {
			return "", nil, fmt.Errorf("invalid %s value %q: duration must be positive", field, value)
		}
		return value, nil, nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		utc := parsed.UTC()
		return utc.Format(time.RFC3339Nano), &utc, nil
	}
	if unixPattern.MatchString(value) {
		seconds, err := strconv.ParseFloat(value, 64)
		if err == nil {
			whole := int64(seconds)
			nanos := int64((seconds - float64(whole)) * 1e9)
			parsed := time.Unix(whole, nanos).UTC()
			return parsed.Format(time.RFC3339Nano), &parsed, nil
		}
	}
	return "", nil, fmt.Errorf("invalid %s value %q: use a duration (10m, 1h30m), an RFC3339 timestamp (2026-10-01T12:00:00Z), or Unix seconds", field, value)
}

var unixPattern = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,9})?$`)

// CompileLogsGrep validates and compiles a --grep pattern (RE2).
func CompileLogsGrep(pattern string, ignoreCase bool) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, nil
	}
	if len(pattern) > MaxLogsGrepBytes {
		return nil, fmt.Errorf("grep pattern exceeds %d bytes", MaxLogsGrepBytes)
	}
	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid grep pattern: %w", err)
	}
	return compiled, nil
}

// validateLogsStreamRequest checks and normalizes a stream request.
func validateLogsStreamRequest(req *LogsStreamRequest) (*regexp.Regexp, error) {
	if strings.TrimSpace(req.Project) == "" {
		return nil, fmt.Errorf("project is required")
	}
	if strings.TrimSpace(req.Environment) == "" {
		return nil, fmt.Errorf("environment is required")
	}
	if !isSafeProjectName(req.Project) {
		return nil, fmt.Errorf("invalid project name")
	}
	if !isSafeRuntimeName(req.Environment) {
		return nil, fmt.Errorf("invalid environment name")
	}
	if len(req.Services) == 0 && len(req.IncludeJobs) == 0 {
		return nil, fmt.Errorf("at least one service is required")
	}
	for _, name := range append(append([]string(nil), req.Services...), req.IncludeJobs...) {
		if !isSafeServiceName(name) {
			return nil, fmt.Errorf("invalid service name %q", name)
		}
	}
	if req.Tail != nil {
		if *req.Tail < 0 {
			return nil, fmt.Errorf("tail cannot be negative")
		}
		if *req.Tail > maxLogTail {
			return nil, fmt.Errorf("tail cannot exceed %d", maxLogTail)
		}
	}
	since, sinceTime, err := NormalizeLogsTimeValue("since", req.Since)
	if err != nil {
		return nil, err
	}
	until, untilTime, err := NormalizeLogsTimeValue("until", req.Until)
	if err != nil {
		return nil, err
	}
	if until != "" && req.Follow {
		return nil, fmt.Errorf("until cannot be combined with follow")
	}
	if sinceTime != nil && untilTime != nil && untilTime.Before(*sinceTime) {
		return nil, fmt.Errorf("until must not be earlier than since")
	}
	req.Since, req.Until = since, until
	return CompileLogsGrep(req.Grep, req.IgnoreCase)
}

// logsSource is one container contributing to a logs stream.
type logsSource struct {
	ID       string
	Name     string
	Service  string
	Slot     string
	Revision string
	Job      bool
}

func (s logsSource) frame(frameType string, event string) LogsStreamFrame {
	return LogsStreamFrame{Type: frameType, Event: event, Service: s.Service, Slot: s.Slot, Container: s.Name, Revision: s.Revision}
}

// logsContainerEvent is the subset of a docker container event the stream
// watcher needs.
type logsContainerEvent struct {
	Action     string
	ID         string
	Attributes map[string]string
	Time       time.Time
}

// Seams for tests: container discovery, `docker logs`, and `docker events`.
var (
	logsListContainers  = listLogsContainers
	logsRunDockerLogs   = runDockerLogsCommand
	logsWatchContainers = watchDockerContainerEvents
)

const logsContainerFormat = `{{.ID}}|{{.Names}}|{{.Label "tako.service"}}|{{.Label "tako.slot"}}|{{.Label "tako.revision"}}|{{.Label "tako.role"}}`

func listLogsContainers(ctx context.Context, project string, environment string) ([]logsContainerRow, error) {
	output, err := runDocker(ctx, "ps", "-a",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
		"--format", logsContainerFormat,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w: %s", err, strings.TrimSpace(output))
	}
	return parseLogsContainerRows(output), nil
}

// logsContainerRow is one parsed `docker ps` row.
type logsContainerRow struct {
	ID       string
	Name     string
	Service  string
	Slot     string
	Revision string
	Role     string
}

func parseLogsContainerRows(output string) []logsContainerRow {
	var rows []logsContainerRow
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 6 || fields[0] == "" {
			continue
		}
		rows = append(rows, logsContainerRow{ID: fields[0], Name: fields[1], Service: fields[2], Slot: fields[3], Revision: fields[4], Role: fields[5]})
	}
	return rows
}

func runDockerLogsCommand(ctx context.Context, args []string, stdout io.Writer, stderr io.Writer) error {
	cmd := dockerCommandContext(ctx, "docker", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func watchDockerContainerEvents(ctx context.Context, project string, environment string, handle func(logsContainerEvent)) error {
	reader, writer := io.Pipe()
	cmd := dockerCommandContext(ctx, "docker", "events",
		"--format", "{{json .}}",
		"--filter", "type=container",
		"--filter", "event=start",
		"--filter", "event=die",
		"--filter", "label=tako.project="+project,
		"--filter", "label=tako.environment="+environment,
	)
	cmd.Stdout = writer
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to watch container events: %w", err)
	}
	waitErr := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		_ = writer.CloseWithError(io.EOF)
		waitErr <- err
	}()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if event, ok := parseDockerContainerEvent(scanner.Bytes()); ok {
			handle(event)
		}
	}
	err := <-waitErr
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("container event watcher failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("container event watcher exited")
}

func parseDockerContainerEvent(data []byte) (logsContainerEvent, bool) {
	var raw struct {
		Status   string `json:"status"`
		Action   string `json:"Action"`
		ID       string `json:"id"`
		TimeNano int64  `json:"timeNano"`
		Time     int64  `json:"time"`
		Actor    struct {
			ID         string            `json:"ID"`
			Attributes map[string]string `json:"Attributes"`
		} `json:"Actor"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return logsContainerEvent{}, false
	}
	event := logsContainerEvent{Action: raw.Action, ID: raw.Actor.ID, Attributes: raw.Actor.Attributes}
	if event.Action == "" {
		event.Action = raw.Status
	}
	if event.ID == "" {
		event.ID = raw.ID
	}
	switch {
	case raw.TimeNano > 0:
		event.Time = time.Unix(0, raw.TimeNano).UTC()
	case raw.Time > 0:
		event.Time = time.Unix(raw.Time, 0).UTC()
	}
	if event.ID == "" || event.Action == "" {
		return logsContainerEvent{}, false
	}
	return event, true
}

// logsFrameWriter serializes whole NDJSON frames from concurrent producers.
type logsFrameWriter struct {
	mu     sync.Mutex
	writer io.Writer
	err    error
}

func (w *logsFrameWriter) write(frame LogsStreamFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if _, err := w.writer.Write(data); err != nil {
		w.err = err
	}
	return w.err
}

// logsLine is one parsed `docker logs -t` line.
type logsLine struct {
	stream string
	time   string
	parsed time.Time
	data   string
}

// parseDockerLogLine splits Docker's RFC3339Nano timestamp prefix from a
// `docker logs -t` line. ok is false when the line has no timestamp (Docker
// CLI diagnostics, not container output).
func parseDockerLogLine(stream string, raw string) (logsLine, bool) {
	raw = strings.TrimRight(raw, "\r\n")
	stamp, rest, found := strings.Cut(raw, " ")
	if parsed, err := time.Parse(time.RFC3339Nano, stamp); err == nil && (found || rest == "") {
		return logsLine{stream: stream, time: stamp, parsed: parsed, data: rest}, true
	}
	return logsLine{stream: stream, data: raw}, false
}

// logsStreamer runs one stream request on this node.
type logsStreamer struct {
	req     LogsStreamRequest
	tail    int
	pattern *regexp.Regexp
	out     *logsFrameWriter
	now     func() time.Time

	services map[string]bool
	jobs     map[string]bool

	mu       sync.Mutex
	attached map[string]*logsAttachment
	wg       sync.WaitGroup
	failOnce sync.Once
	failErr  error
	cancel   context.CancelFunc
}

type logsAttachment struct {
	source  logsSource
	restart *time.Time
}

// StreamServiceLogFrames validates req and writes NDJSON frames to writer
// until the stream completes (non-follow) or ctx ends (follow). Validation
// failures are returned before anything is written; the caller reports later
// failures with an error frame.
func StreamServiceLogFrames(ctx context.Context, req LogsStreamRequest, writer io.Writer) error {
	pattern, err := validateLogsStreamRequest(&req)
	if err != nil {
		return err
	}
	return newLogsStreamer(req, pattern, writer).run(ctx)
}

func newLogsStreamer(req LogsStreamRequest, pattern *regexp.Regexp, writer io.Writer) *logsStreamer {
	tail := defaultLogTail
	if req.Tail != nil {
		tail = *req.Tail
	}
	s := &logsStreamer{
		req:      req,
		tail:     tail,
		pattern:  pattern,
		out:      &logsFrameWriter{writer: writer},
		now:      time.Now,
		services: map[string]bool{},
		jobs:     map[string]bool{},
		attached: map[string]*logsAttachment{},
	}
	for _, name := range req.Services {
		s.services[name] = true
	}
	for _, name := range req.IncludeJobs {
		s.jobs[name] = true
	}
	return s
}

// selectSource maps a container's labels to a source when it belongs to the
// request: long-running service containers carry no tako.role label; job
// run containers carry tako.role=job. Exec and backup one-offs are skipped.
func (s *logsStreamer) selectSource(id, name, service, slot, revision, role string) (logsSource, bool) {
	source := logsSource{ID: id, Name: name, Service: service, Slot: slot, Revision: revision}
	switch {
	case role == "" && s.services[service]:
		return source, true
	case role == "job" && s.jobs[service]:
		source.Job = true
		source.Slot = LogsJobSlot
		return source, true
	}
	return logsSource{}, false
}

func (s *logsStreamer) discover(ctx context.Context) ([]logsSource, error) {
	rows, err := logsListContainers(ctx, s.req.Project, s.req.Environment)
	if err != nil {
		return nil, err
	}
	var sources []logsSource
	for _, row := range rows {
		if source, ok := s.selectSource(row.ID, row.Name, row.Service, row.Slot, row.Revision, row.Role); ok {
			sources = append(sources, source)
		}
	}
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i].Service != sources[j].Service {
			return sources[i].Service < sources[j].Service
		}
		left, leftErr := strconv.Atoi(sources[i].Slot)
		right, rightErr := strconv.Atoi(sources[j].Slot)
		if leftErr == nil && rightErr == nil && left != right {
			return left < right
		}
		return sources[i].Name < sources[j].Name
	})
	return sources, nil
}

func (s *logsStreamer) run(ctx context.Context) error {
	if !s.req.Follow {
		sources, err := s.discover(ctx)
		if err != nil {
			return err
		}
		count := len(sources)
		if err := s.out.write(LogsStreamFrame{Type: LogsFrameReady, Sources: &count}); err != nil {
			return err
		}
		for _, source := range sources {
			if err := s.streamHistory(ctx, source); err != nil {
				return err
			}
		}
		return nil
	}
	return s.follow(ctx)
}

// streamHistory streams one container without follow.
func (s *logsStreamer) streamHistory(ctx context.Context, source logsSource) error {
	if err := s.out.write(source.frame(LogsFrameSource, LogsSourceAttached)); err != nil {
		return err
	}
	if err := s.emitHistory(ctx, source, s.req.Until); err != nil {
		return err
	}
	return s.out.write(source.frame(LogsFrameSource, LogsSourceDetached))
}

// emitHistory collects a bounded window of a container's lines, merges
// stdout/stderr by timestamp, applies grep, and emits the last tail lines
// (counting matches when grep is set).
func (s *logsStreamer) emitHistory(ctx context.Context, source logsSource, until string) error {
	scan := s.tail
	if s.pattern != nil {
		scan = maxLogTail
	}
	args := []string{"logs", "-t", "--tail", strconv.Itoa(scan)}
	if s.req.Since != "" {
		args = append(args, "--since", s.req.Since)
	}
	if until != "" {
		args = append(args, "--until", until)
	}
	args = append(args, source.ID)

	var mu sync.Mutex
	var stdoutLines, stderrLines []logsLine
	err := s.runLogs(ctx, source, args, func(line logsLine) {
		mu.Lock()
		defer mu.Unlock()
		if line.stream == "stderr" {
			stderrLines = append(stderrLines, line)
		} else {
			stdoutLines = append(stdoutLines, line)
		}
	})
	if err != nil {
		return err
	}
	lines := mergeLogsLines(stdoutLines, stderrLines)
	if len(lines) > s.tail {
		lines = lines[len(lines)-s.tail:]
	}
	for _, line := range lines {
		if err := s.emitLine(source, line); err != nil {
			return err
		}
	}
	return nil
}

func mergeLogsLines(left, right []logsLine) []logsLine {
	merged := make([]logsLine, 0, len(left)+len(right))
	i, j := 0, 0
	for i < len(left) && j < len(right) {
		if right[j].parsed.Before(left[i].parsed) {
			merged = append(merged, right[j])
			j++
			continue
		}
		merged = append(merged, left[i])
		i++
	}
	merged = append(merged, left[i:]...)
	return append(merged, right[j:]...)
}

func (s *logsStreamer) emitLine(source logsSource, line logsLine) error {
	return s.out.write(LogsStreamFrame{
		Type:      LogsFrameLine,
		Service:   source.Service,
		Slot:      source.Slot,
		Container: source.Name,
		Revision:  source.Revision,
		Stream:    line.stream,
		Time:      line.time,
		Data:      line.data,
	})
}

// runLogs runs one `docker logs` invocation, delivering filtered container
// lines to handle. A container removed meanwhile ends quietly.
func (s *logsStreamer) runLogs(ctx context.Context, source logsSource, args []string, handle func(logsLine)) error {
	var diagnostics []string
	var diagMu sync.Mutex
	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()
	var readers sync.WaitGroup
	read := func(stream string, reader io.Reader) {
		defer readers.Done()
		buffered := bufio.NewReader(reader)
		for {
			raw, err := buffered.ReadString('\n')
			if raw != "" {
				line, ok := parseDockerLogLine(stream, raw)
				switch {
				case !ok && stream == "stderr":
					diagMu.Lock()
					diagnostics = append(diagnostics, line.data)
					diagMu.Unlock()
				case s.pattern == nil || s.pattern.MatchString(line.data):
					handle(line)
				}
			}
			if err != nil {
				_, _ = io.Copy(io.Discard, reader)
				return
			}
		}
	}
	readers.Add(2)
	go read("stdout", stdoutReader)
	go read("stderr", stderrReader)
	runErr := logsRunDockerLogs(ctx, args, stdoutWriter, stderrWriter)
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	readers.Wait()
	if runErr == nil || ctx.Err() != nil {
		return nil
	}
	message := strings.TrimSpace(strings.Join(diagnostics, "\n"))
	if strings.Contains(message, "No such container") {
		return nil
	}
	if message == "" {
		message = runErr.Error()
	}
	return fmt.Errorf("failed to stream logs for %s: %s", source.Name, message)
}

// follow attaches every current matching container, then watches docker
// events so containers started later (deploys, restarts, job runs) attach
// without restarting the stream. It returns when ctx ends or on failure.
func (s *logsStreamer) follow(parent context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	s.cancel = cancel

	watchErr := make(chan error, 1)
	watcherStarted := make(chan struct{})
	go func() {
		close(watcherStarted)
		watchErr <- logsWatchContainers(ctx, s.req.Project, s.req.Environment, func(event logsContainerEvent) {
			s.handleEvent(ctx, event)
		})
	}()
	<-watcherStarted

	sources, err := s.discover(ctx)
	if err != nil {
		cancel()
		<-watchErr
		s.wg.Wait()
		return err
	}
	for _, source := range sources {
		s.attach(ctx, source, nil)
	}
	count := len(sources)
	if err := s.out.write(LogsStreamFrame{Type: LogsFrameReady, Sources: &count}); err != nil {
		s.fail(err)
	}

	select {
	case <-ctx.Done():
		<-watchErr
	case err := <-watchErr:
		if err != nil {
			s.fail(err)
		}
		<-ctx.Done()
	}
	// The watcher has returned, so no new follower can be added while
	// waiting; restarts are only re-attached by live followers.
	s.wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failErr
}

func (s *logsStreamer) fail(err error) {
	if err == nil {
		return
	}
	s.failOnce.Do(func() {
		s.mu.Lock()
		s.failErr = err
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
	})
}

func (s *logsStreamer) handleEvent(ctx context.Context, event logsContainerEvent) {
	attrs := event.Attributes
	source, ok := s.selectSource(event.ID, attrs["name"], attrs["tako.service"], attrs["tako.slot"], attrs["tako.revision"], attrs["tako.role"])
	if !ok {
		return
	}
	switch event.Action {
	case "start":
		since := event.Time
		if since.IsZero() {
			since = s.now().UTC()
		}
		s.attach(ctx, source, &since)
	case "die":
		if !source.Job {
			return
		}
		frame := source.frame(LogsFrameRun, LogsRunFinished)
		frame.Job = source.Service
		if code, err := strconv.Atoi(attrs["exitCode"]); err == nil {
			frame.ExitCode = &code
		}
		if err := s.out.write(frame); err != nil {
			s.fail(err)
		}
	}
}

// attach starts following one container. since is nil for containers
// discovered at stream start (history honors tail/since) and the container
// start time for containers that started later.
func (s *logsStreamer) attach(ctx context.Context, source logsSource, since *time.Time) {
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if existing, ok := s.attached[source.ID]; ok {
		if since != nil {
			// Restarted while the previous follower is still draining:
			// re-attach once it ends.
			existing.restart = since
		}
		s.mu.Unlock()
		return
	}
	s.attached[source.ID] = &logsAttachment{source: source}
	s.mu.Unlock()

	if source.Job {
		frame := source.frame(LogsFrameRun, LogsRunStarted)
		frame.Job = source.Service
		if err := s.out.write(frame); err != nil {
			s.fail(err)
			return
		}
	}
	if err := s.out.write(source.frame(LogsFrameSource, LogsSourceAttached)); err != nil {
		s.fail(err)
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := s.followContainer(ctx, source, since)
		if err != nil && ctx.Err() == nil {
			s.fail(err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err := s.out.write(source.frame(LogsFrameSource, LogsSourceDetached)); err != nil {
			s.fail(err)
			return
		}
		s.mu.Lock()
		restart := s.attached[source.ID].restart
		delete(s.attached, source.ID)
		s.mu.Unlock()
		if restart != nil {
			s.attach(ctx, source, restart)
		}
	}()
}

// followContainer streams one container until it stops or ctx ends. With
// grep, historical matches are emitted first (bounded, last tail matches),
// then live lines are filtered as they arrive.
func (s *logsStreamer) followContainer(ctx context.Context, source logsSource, since *time.Time) error {
	emit := func(line logsLine) {
		if err := s.emitLine(source, line); err != nil {
			s.fail(err)
		}
	}
	if since != nil {
		args := []string{"logs", "-t", "-f", "--since", since.UTC().Format(time.RFC3339Nano), source.ID}
		return s.runLogs(ctx, source, args, emit)
	}
	if s.pattern == nil {
		args := []string{"logs", "-t", "--tail", strconv.Itoa(s.tail), "-f"}
		if s.req.Since != "" {
			args = append(args, "--since", s.req.Since)
		}
		args = append(args, source.ID)
		return s.runLogs(ctx, source, args, emit)
	}
	boundary := s.now().UTC().Format(time.RFC3339Nano)
	if err := s.emitHistory(ctx, source, boundary); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return nil
	}
	args := []string{"logs", "-t", "-f", "--since", boundary, source.ID}
	return s.runLogs(ctx, source, args, emit)
}
