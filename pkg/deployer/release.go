package deployer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/runtimeid"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// DefaultReleaseTimeout bounds a release command when the config sets none.
const DefaultReleaseTimeout = 5 * time.Minute

// releaseStreamGrace keeps the client deadline behind takod's server-side
// timeout so the terminal exit frame arrives before the stream is cut.
const releaseStreamGrace = 30 * time.Second

// ReleaseRun records one executed release command for the deploy result.
type ReleaseRun struct {
	Service    string
	Server     string
	Image      string
	Command    []string
	ExitCode   int
	DurationMs int64
}

// SetEventSink routes release-command lifecycle events to an engine event
// stream; without one, event messages fall back to the progress output.
func (d *Deployer) SetEventSink(sink events.Sink) {
	d.events = sink
}

// ReleaseRunFor returns the recorded release run for a service in this
// deploy, or nil when no release command ran.
func (d *Deployer) ReleaseRunFor(serviceName string) *ReleaseRun {
	d.releaseMu.Lock()
	defer d.releaseMu.Unlock()
	return d.releaseRuns[serviceName]
}

func (d *Deployer) recordReleaseRun(run *ReleaseRun) {
	d.releaseMu.Lock()
	defer d.releaseMu.Unlock()
	if d.releaseRuns == nil {
		d.releaseRuns = make(map[string]*ReleaseRun)
	}
	d.releaseRuns[run.Service] = run
}

func (d *Deployer) emitEvent(event events.Event) {
	if d.events != nil {
		d.events.Emit(event)
		return
	}
	if event.Message != "" {
		d.printf("%s", event.Message)
	}
}

// runReleaseCommand executes the service's deploy.release command exactly
// once per deploy: a one-off container from the NEW image with the
// service's env/secrets and network, on the first assigned node, before any
// rollout activation. A non-zero exit aborts the deploy.
func (d *Deployer) runReleaseCommand(serviceName string, service *config.ServiceConfig, imageRef string, serverNames []string) error {
	release := service.Deploy.Release
	if release == nil {
		return nil
	}
	if len(serverNames) == 0 {
		return fmt.Errorf("service %s has no assigned nodes to run the release command", serviceName)
	}
	serverName := serverNames[0]

	timeout := DefaultReleaseTimeout
	if strings.TrimSpace(release.Timeout) != "" {
		parsed, err := time.ParseDuration(release.Timeout)
		if err != nil {
			return fmt.Errorf("service %s: invalid deploy.release.timeout: %w", serviceName, err)
		}
		timeout = parsed
	}

	client, err := d.getEnvironmentClient(serverName)
	if err != nil {
		return fmt.Errorf("failed to connect to node %s for release command: %w", serverName, err)
	}
	envContent, err := d.buildTakodEnvFileContent(service)
	if err != nil {
		return fmt.Errorf("failed to build release env for %s: %w", serviceName, err)
	}
	var mounts []string
	if release.Volumes {
		mounts, _, err = d.buildTakodMountSpecs(serviceName, service)
		if err != nil {
			return fmt.Errorf("failed to resolve release mounts for %s: %w", serviceName, err)
		}
	}
	fileBundles, fileMounts, _, err := d.PrepareServiceFiles(serviceName, service)
	if err != nil {
		return err
	}
	mounts = append(mounts, fileMounts...)
	fileSetID := ""
	if len(service.Files) > 0 {
		fileSetID, err = serviceFileSetID(service.FilesContentHash)
		if err != nil {
			return err
		}
	}

	request := takod.ExecRequest{
		Project:        d.config.Project.Name,
		Environment:    d.environment,
		Service:        serviceName,
		Mode:           takod.ExecModeOneOff,
		Command:        append([]string(nil), release.Command...),
		Image:          imageRef,
		EnvFileContent: envContent,
		Network:        runtimeid.NetworkName(d.config.Project.Name, d.environment),
		Mounts:         mounts,
		Files:          fileBundles,
		FileSetID:      fileSetID,
		TimeoutSeconds: int(timeout / time.Second),
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("failed to encode release request: %w", err)
	}

	d.emitEvent(events.Event{
		Type:    events.TypeDeployReleaseStarted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelInfo,
		Service: serviceName,
		Node:    serverName,
		Message: fmt.Sprintf("→ Running release command for %s on %s: %s\n", serviceName, serverName, strings.Join(release.Command, " ")),
		Data:    map[string]any{"command": request.Command, "image": imageRef, "node": serverName},
	})

	ctx, cancel := context.WithTimeout(d.baseContext(), timeout+releaseStreamGrace)
	defer cancel()

	started := time.Now()
	exitCode, exitSeen, streamErr := d.streamDeployExec(ctx, client, serviceName, serverName, payload, events.TypeDeployReleaseOutput)
	run := &ReleaseRun{
		Service:    serviceName,
		Server:     serverName,
		Image:      imageRef,
		Command:    append([]string(nil), release.Command...),
		ExitCode:   exitCode,
		DurationMs: time.Since(started).Milliseconds(),
	}
	d.recordReleaseRun(run)

	var runErr error
	switch {
	case streamErr != nil:
		runErr = fmt.Errorf("release command for %s failed: %w", serviceName, streamErr)
	case !exitSeen:
		runErr = fmt.Errorf("release command for %s ended without an exit status", serviceName)
	case exitCode != 0:
		runErr = fmt.Errorf("release command for %s exited with code %d; rollout aborted before cutover", serviceName, exitCode)
	}
	if runErr != nil {
		d.emitEvent(events.Event{
			Type:    events.TypeDeployReleaseFailed,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelError,
			Service: serviceName,
			Node:    serverName,
			Message: fmt.Sprintf("  ✗ Release command failed (exit %d)\n", exitCode),
			Data:    map[string]any{"exitCode": exitCode, "durationMs": run.DurationMs},
		})
		return runErr
	}

	d.emitEvent(events.Event{
		Type:    events.TypeDeployReleaseCompleted,
		Phase:   events.PhaseDeploy,
		Level:   events.LevelInfo,
		Service: serviceName,
		Node:    serverName,
		Message: fmt.Sprintf("  ✓ Release command completed in %.1fs\n", float64(run.DurationMs)/1000),
		Data:    map[string]any{"exitCode": exitCode, "durationMs": run.DurationMs},
	})
	return nil
}

// streamReleaseExec streams the exec response, forwarding output lines as
// deploy.release.output events and parsing the marker frames.
func (d *Deployer) streamReleaseExec(ctx context.Context, client takodclient.StreamExecutor, serviceName string, serverName string, payload []byte) (int, bool, error) {
	return d.streamDeployExec(ctx, client, serviceName, serverName, payload, events.TypeDeployReleaseOutput)
}

func (d *Deployer) streamDeployExec(ctx context.Context, client takodclient.StreamExecutor, serviceName string, serverName string, payload []byte, outputEventType string) (int, bool, error) {
	reader, writer := io.Pipe()
	streamDone := make(chan error, 1)
	go func() {
		err := takodclient.StreamRequestOutputWithContext(ctx, client, d.takodSocket(), "POST", takodclient.ExecEndpoint(), strings.NewReader(string(payload)), writer, writer)
		if err != nil {
			_ = writer.CloseWithError(err)
		} else {
			_ = writer.Close()
		}
		streamDone <- err
	}()

	exitCode := -1
	exitSeen := false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if _, ok := strings.CutPrefix(line, takod.ExecContainerMarker); ok {
			continue
		}
		if code, ok := strings.CutPrefix(line, takod.ExecExitMarker); ok {
			if parsed, err := strconv.Atoi(strings.TrimSpace(code)); err == nil {
				exitCode = parsed
				exitSeen = true
			}
			continue
		}
		d.emitEvent(events.Event{
			Type:    outputEventType,
			Phase:   events.PhaseDeploy,
			Level:   events.LevelInfo,
			Service: serviceName,
			Node:    serverName,
			Message: "  " + line + "\n",
			Data:    map[string]any{"data": line},
		})
	}
	scanErr := scanner.Err()
	streamErr := <-streamDone
	if streamErr != nil {
		return exitCode, exitSeen, streamErr
	}
	return exitCode, exitSeen, scanErr
}
