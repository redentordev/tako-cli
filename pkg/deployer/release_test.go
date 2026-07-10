package deployer

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takod"
)

// fakeExecStreamExecutor plays back a canned exec response stream.
type fakeExecStreamExecutor struct {
	response string
}

func (f *fakeExecStreamExecutor) ExecuteWithContext(ctx context.Context, cmd string) (string, error) {
	return "", nil
}

func (f *fakeExecStreamExecutor) ExecuteWithInput(ctx context.Context, cmd string, input io.Reader) (string, error) {
	return "", nil
}

func (f *fakeExecStreamExecutor) ExecuteStream(cmd string, stdout, stderr io.Writer) error {
	_, err := io.WriteString(stdout, f.response)
	return err
}

type capturingSink struct {
	events []events.Event
}

func (s *capturingSink) Emit(event events.Event) {
	s.events = append(s.events, event)
}

func releaseTestDeployer(sink events.Sink) *Deployer {
	d := NewDeployer(nil, &config.Config{Project: config.ProjectConfig{Name: "demo"}}, "production", false)
	if sink != nil {
		d.SetEventSink(sink)
	} else {
		d.SetOutput(&bytes.Buffer{})
	}
	return d
}

func TestStreamReleaseExecParsesMarkersAndEmitsOutput(t *testing.T) {
	sink := &capturingSink{}
	d := releaseTestDeployer(sink)
	client := &fakeExecStreamExecutor{response: strings.Join([]string{
		takod.ExecContainerMarker + "tako_demo_production_web_exec_1",
		"Migrating: 2026_07_06_create_users",
		"Migrated:  2026_07_06_create_users",
		takod.ExecExitMarker + "0",
		"",
	}, "\n")}

	exitCode, exitSeen, err := d.streamReleaseExec(context.Background(), client, "web", "node-a", []byte("{}"))
	if err != nil {
		t.Fatalf("streamReleaseExec: %v", err)
	}
	if !exitSeen || exitCode != 0 {
		t.Fatalf("exit = %d (seen %v), want 0 seen", exitCode, exitSeen)
	}

	var outputs []string
	for _, event := range sink.events {
		if event.Type != events.TypeDeployReleaseOutput {
			t.Fatalf("unexpected event type %s", event.Type)
		}
		outputs = append(outputs, event.Data["data"].(string))
	}
	if len(outputs) != 2 || !strings.HasPrefix(outputs[0], "Migrating") {
		t.Fatalf("outputs = %#v, want two migration lines", outputs)
	}
}

func TestStreamDeployExecUsesRunOutputEvent(t *testing.T) {
	sink := &capturingSink{}
	d := releaseTestDeployer(sink)
	client := &fakeExecStreamExecutor{response: "bootstrapping\n" + takod.ExecExitMarker + "0\n"}
	exitCode, exitSeen, err := d.streamDeployExec(context.Background(), client, "bootstrap", "node-a", []byte("{}"), events.TypeDeployRunOutput)
	if err != nil || !exitSeen || exitCode != 0 {
		t.Fatalf("stream = %d %v %v", exitCode, exitSeen, err)
	}
	if len(sink.events) != 1 || sink.events[0].Type != events.TypeDeployRunOutput {
		t.Fatalf("events = %#v", sink.events)
	}
}

func TestStreamReleaseExecReportsNonZeroExit(t *testing.T) {
	d := releaseTestDeployer(&capturingSink{})
	client := &fakeExecStreamExecutor{response: takod.ExecExitMarker + "1\n"}

	exitCode, exitSeen, err := d.streamReleaseExec(context.Background(), client, "web", "node-a", []byte("{}"))
	if err != nil {
		t.Fatalf("streamReleaseExec: %v", err)
	}
	if !exitSeen || exitCode != 1 {
		t.Fatalf("exit = %d (seen %v), want 1 seen", exitCode, exitSeen)
	}
}

func TestStreamReleaseExecMissingExitFrame(t *testing.T) {
	d := releaseTestDeployer(&capturingSink{})
	client := &fakeExecStreamExecutor{response: "partial output\n"}

	_, exitSeen, err := d.streamReleaseExec(context.Background(), client, "web", "node-a", []byte("{}"))
	if err != nil {
		t.Fatalf("streamReleaseExec: %v", err)
	}
	if exitSeen {
		t.Fatal("exitSeen = true without exit frame")
	}
}

func TestRunReleaseCommandSkipsWithoutConfig(t *testing.T) {
	d := releaseTestDeployer(nil)
	service := &config.ServiceConfig{}
	if err := d.runReleaseCommand("web", service, "img", []string{"node-a"}); err != nil {
		t.Fatalf("runReleaseCommand without release block: %v", err)
	}
	if d.ReleaseRunFor("web") != nil {
		t.Fatal("release run recorded without release block")
	}
}

func TestRunReleaseCommandRejectsInvalidTimeout(t *testing.T) {
	d := releaseTestDeployer(nil)
	service := &config.ServiceConfig{Deploy: config.DeployConfig{Release: &config.ReleaseConfig{Command: []string{"true"}, Timeout: "soon"}}}
	err := d.runReleaseCommand("web", service, "img", []string{"node-a"})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout validation error", err)
	}
}
