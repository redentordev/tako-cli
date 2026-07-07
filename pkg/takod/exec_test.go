package takod

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func validExecRequestFixture() ExecRequest {
	return ExecRequest{
		Project:     "demo",
		Environment: "production",
		Service:     "web",
		Mode:        ExecModeAttach,
		Command:     []string{"env"},
	}
}

func TestValidateExecRequestAcceptsAttachAndOneOff(t *testing.T) {
	req := validExecRequestFixture()
	if err := validateExecRequest(&req); err != nil {
		t.Fatalf("attach request rejected: %v", err)
	}
	req.Mode = ExecModeOneOff
	req.Image = "registry.example.com/web:abc123"
	req.Mounts = []string{"type=volume,source=tako_demo_production_data,target=/data"}
	req.Env = []string{"RAILS_ENV=production"}
	if err := validateExecRequest(&req); err != nil {
		t.Fatalf("oneoff request rejected: %v", err)
	}
}

func TestValidateExecRequestRejectsBadInput(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ExecRequest)
	}{
		{"bad project", func(r *ExecRequest) { r.Project = "Demo!" }},
		{"bad environment", func(r *ExecRequest) { r.Environment = "prod$" }},
		{"bad service", func(r *ExecRequest) { r.Service = "Web" }},
		{"bad mode", func(r *ExecRequest) { r.Mode = "interactive" }},
		{"empty command", func(r *ExecRequest) { r.Command = nil }},
		{"blank command", func(r *ExecRequest) { r.Command = []string{"  "} }},
		{"env without equals", func(r *ExecRequest) { r.Env = []string{"NOVALUE"} }},
		{"env control chars", func(r *ExecRequest) { r.Env = []string{"KEY=a\nb"} }},
		{"bad network", func(r *ExecRequest) { r.Network = "net work" }},
		{"blank mount", func(r *ExecRequest) { r.Mounts = []string{" "} }},
		{"negative replica", func(r *ExecRequest) { r.Replica = -1 }},
		{"excessive timeout", func(r *ExecRequest) { r.TimeoutSeconds = maxExecTimeoutSeconds + 1 }},
		{"oversized cols", func(r *ExecRequest) { r.PTY = true; r.Cols = maxExecTerminalDim + 1 }},
		{"negative rows", func(r *ExecRequest) { r.PTY = true; r.Rows = -1 }},
		{"size without pty", func(r *ExecRequest) { r.Cols = 80; r.Rows = 24 }},
		{"idle timeout without interactive", func(r *ExecRequest) { r.IdleTimeoutSeconds = 60 }},
		{"excessive idle timeout", func(r *ExecRequest) { r.Interactive = true; r.IdleTimeoutSeconds = maxExecTimeoutSeconds + 1 }},
	}
	for _, tc := range cases {
		req := validExecRequestFixture()
		tc.mutate(&req)
		if err := validateExecRequest(&req); err == nil {
			t.Fatalf("%s: request accepted", tc.name)
		}
	}
}

func TestBuildExecAttachArgs(t *testing.T) {
	req := validExecRequestFixture()
	req.Env = []string{"FOO=bar"}
	req.Command = []string{"sh", "-c", "echo hi"}

	got := buildExecAttachArgs(req, "tako_demo_production_web_1")
	want := []string{"exec", "-e", "FOO=bar", "tako_demo_production_web_1", "sh", "-c", "echo hi"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestValidateExecRequestPTYImpliesInteractive(t *testing.T) {
	req := validExecRequestFixture()
	req.PTY = true
	req.Cols = 120
	req.Rows = 40
	if err := validateExecRequest(&req); err != nil {
		t.Fatalf("pty request rejected: %v", err)
	}
	if !req.Interactive {
		t.Fatal("pty must imply interactive")
	}
}

func TestBuildExecArgsInteractiveFlags(t *testing.T) {
	req := validExecRequestFixture()
	req.Interactive = true
	got := buildExecAttachArgs(req, "c1")
	if !slices.Equal(got[:2], []string{"exec", "-i"}) {
		t.Fatalf("interactive attach args = %#v, want exec -i prefix", got)
	}

	req.PTY = true
	got = buildExecAttachArgs(req, "c1")
	if !slices.Equal(got[:3], []string{"exec", "-i", "-t"}) {
		t.Fatalf("pty attach args = %#v, want exec -i -t prefix", got)
	}

	req.Mode = ExecModeOneOff
	got = buildExecOneOffArgs(req, "c1", "img", "")
	if !slices.Equal(got[:4], []string{"run", "--rm", "-i", "-t"}) {
		t.Fatalf("pty oneoff args = %#v, want run --rm -i -t prefix", got)
	}

	// Non-interactive stays byte-identical: no -i/-t anywhere.
	plain := validExecRequestFixture()
	got = buildExecAttachArgs(plain, "c1")
	if slices.Contains(got, "-i") || slices.Contains(got, "-t") {
		t.Fatalf("non-interactive args gained tty flags: %#v", got)
	}
}

func TestBuildExecOneOffArgsLabelsAndDefaults(t *testing.T) {
	req := validExecRequestFixture()
	req.Mode = ExecModeOneOff
	req.Mounts = []string{"type=volume,source=data,target=/data"}

	got := buildExecOneOffArgs(req, "tako_demo_production_web_exec_1", "registry.example.com/web:abc", "/tmp/envfile")

	joined := strings.Join(got, " ")
	for _, want := range []string{
		"run --rm",
		"--name tako_demo_production_web_exec_1",
		"--network tako_demo_production",
		"--label tako.role=exec",
		"--label tako.project=demo",
		"--env-file /tmp/envfile",
		"--mount type=volume,source=data,target=/data",
		"registry.example.com/web:abc env",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
}

func TestBuildExecOneOffArgsHonorsExplicitNetwork(t *testing.T) {
	req := validExecRequestFixture()
	req.Mode = ExecModeOneOff
	req.Network = "custom_net"

	got := strings.Join(buildExecOneOffArgs(req, "c", "img", ""), " ")
	if !strings.Contains(got, "--network custom_net") {
		t.Fatalf("args %q missing custom network", got)
	}
	if strings.Contains(got, "--env-file") {
		t.Fatalf("args %q has env-file without content", got)
	}
}

func TestLineStartWriterKeepsMarkersOnOwnLine(t *testing.T) {
	var buf bytes.Buffer
	w := &lineStartWriter{writer: &buf}
	w.Write([]byte("partial output without newline"))
	w.ensureLineStart()
	w.Write([]byte(ExecExitMarker + "0\n"))

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	last := lines[len(lines)-1]
	if last != ExecExitMarker+"0" {
		t.Fatalf("last line = %q, want exit marker line", last)
	}
}
