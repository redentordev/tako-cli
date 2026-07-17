package takod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestInspectImageReturnsDigestPlatformAndDaemonIdentity(t *testing.T) {
	old := dockerCommandContext
	t.Cleanup(func() { dockerCommandContext = old })
	dockerCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		var output string
		switch {
		case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
			output = `[{"Id":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","RepoDigests":["demo/web@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"],"Os":"linux","Architecture":"amd64"}]`
		case len(args) >= 1 && args[0] == "info":
			output = `"daemon-identity"`
		}
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+output+"'")
	}
	descriptor, err := InspectImage(context.Background(), "demo/web:revision")
	if err != nil {
		t.Fatal(err)
	}
	if !descriptor.Exists || descriptor.ImageID != "sha256:"+strings.Repeat("a", 64) || descriptor.OS != "linux" || descriptor.Architecture != "amd64" || descriptor.DaemonID != "daemon-identity" {
		t.Fatalf("descriptor = %#v", descriptor)
	}
}

func TestInspectImageFailsClosedWhenDaemonUnavailable(t *testing.T) {
	old := dockerCommandContext
	t.Cleanup(func() { dockerCommandContext = old })
	dockerCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "exit 1")
	}
	if descriptor, err := InspectImage(context.Background(), "demo/web:revision"); err == nil || descriptor != nil {
		t.Fatalf("daemon failure returned descriptor %#v, %v", descriptor, err)
	}
}

func TestImageExportSavesVerifiedImmutableID(t *testing.T) {
	old := dockerCommandContext
	t.Cleanup(func() { dockerCommandContext = old })
	expected := "sha256:" + strings.Repeat("a", 64)
	var saved string
	dockerCommandContext = func(ctx context.Context, _ string, args ...string) *exec.Cmd {
		var output string
		switch {
		case len(args) >= 2 && args[0] == "image" && args[1] == "inspect":
			output = `[{"Id":"` + expected + `","Os":"linux","Architecture":"amd64"}]`
		case len(args) >= 1 && args[0] == "info":
			output = `"daemon-identity"`
		case len(args) == 2 && args[0] == "save":
			saved = args[1]
			output = "archive"
		}
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+output+"'")
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/images/export?image=demo%2Fweb%3Arevision&expectedImageId="+expected, nil)
	recorder := httptest.NewRecorder()
	(&Server{}).handleImageExport(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export response = %d %q", recorder.Code, recorder.Body.String())
	}
	if saved != expected {
		t.Fatalf("docker save reference = %q, want immutable %q", saved, expected)
	}
}
