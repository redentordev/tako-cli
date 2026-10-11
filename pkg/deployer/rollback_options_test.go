package deployer

import (
	"errors"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/internal/state"
	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func rolloutPhases(skipRelease bool) []string {
	var phases []string
	record := func(name string) func() error {
		return func() error {
			phases = append(phases, name)
			return nil
		}
	}
	_ = runTakodServiceRollout(false, takodServiceRolloutOperations{
		Preflight: record("preflight"),
		Build:     record("build"),
		Release:   takodReleaseOperation(skipRelease, record("release")),
		Reconcile: record("reconcile"),
	})
	return phases
}

func TestTakodRolloutSkipsReleaseOnlyWhenRequested(t *testing.T) {
	if got := strings.Join(rolloutPhases(false), ","); got != "build,release,reconcile" {
		t.Fatalf("default phases = %q", got)
	}
	if got := strings.Join(rolloutPhases(true), ","); got != "build,reconcile" {
		t.Fatalf("skip-release phases = %q", got)
	}
}

func TestRollbackDeployOptions(t *testing.T) {
	buildBacked := &config.ServiceConfig{Build: "."}
	registry := &config.ServiceConfig{Image: "nginx:1"}
	plain := &state.ServiceState{Image: "demo/web:old"}
	shared := &state.ServiceState{Image: "demo/shared/app:old", SharedBuildHash: "h"}

	options, transfer := rollbackDeployOptions(buildBacked, plain, RollbackOptions{ImageSource: RollbackImageSourceRetained})
	if !transfer || options.BuildImage || options.PullImage {
		t.Fatalf("retained build-backed options = %#v transfer=%v, want exact transfer and no build", options, transfer)
	}

	options, transfer = rollbackDeployOptions(buildBacked, plain, RollbackOptions{ImageSource: "rebuild"})
	if transfer || !options.BuildImage {
		t.Fatalf("rebuild options = %#v transfer=%v, want a build", options, transfer)
	}
	options, transfer = rollbackDeployOptions(buildBacked, plain, RollbackOptions{})
	if transfer || !options.BuildImage {
		t.Fatalf("legacy build-backed options = %#v transfer=%v, want a build", options, transfer)
	}
	options, transfer = rollbackDeployOptions(registry, plain, RollbackOptions{ImageSource: "pull", SkipRelease: true})
	if transfer || !options.PullImage || options.BuildImage || !options.SkipRelease {
		t.Fatalf("pull options = %#v transfer=%v", options, transfer)
	}
	options, transfer = rollbackDeployOptions(buildBacked, shared, RollbackOptions{})
	if !transfer || options.BuildImage || options.PullImage || options.SkipRelease {
		t.Fatalf("shared-build options = %#v transfer=%v", options, transfer)
	}
}

func imageDescriptor(id string) *takod.ImageDescriptor {
	return &takod.ImageDescriptor{Image: "demo/web:old", Exists: true, ImageID: id, OS: "linux", Architecture: "amd64", DaemonID: "d"}
}

func TestLocateRetainedImageSources(t *testing.T) {
	allGroups := map[string][]string{"linux/amd64": {"node-b", "node-a"}, "linux/arm64": {"node-c"}}
	inspectFrom := func(descriptors map[string]*takod.ImageDescriptor) func(string, string) (*takod.ImageDescriptor, error) {
		return func(server, _ string) (*takod.ImageDescriptor, error) {
			if descriptor, ok := descriptors[server]; ok {
				return descriptor, nil
			}
			return nil, errors.New("not reachable")
		}
	}

	located, err := locateRetainedImageSources("demo/web:old", allGroups, map[string][]string{"linux/amd64": {"node-b"}}, inspectFrom(map[string]*takod.ImageDescriptor{
		"node-a": imageDescriptor("sha256:1"),
		"node-b": {Image: "demo/web:old"},
	}))
	if err != nil || !retainedOnEveryPlatform(located) || located[0].Server != "node-a" {
		t.Fatalf("found: located = %#v err = %v", located, err)
	}

	located, err = locateRetainedImageSources("demo/web:old", allGroups, map[string][]string{"linux/amd64": {"node-a"}, "linux/arm64": {"node-c"}}, inspectFrom(map[string]*takod.ImageDescriptor{
		"node-a": imageDescriptor("sha256:1"),
	}))
	if err != nil || retainedOnEveryPlatform(located) {
		t.Fatalf("missing on arm64: located = %#v err = %v", located, err)
	}

	_, err = locateRetainedImageSources("demo/web:old", allGroups, map[string][]string{"linux/amd64": {"node-a"}}, inspectFrom(map[string]*takod.ImageDescriptor{
		"node-a": imageDescriptor("sha256:1"),
		"node-b": imageDescriptor("sha256:2"),
	}))
	if err == nil || !strings.Contains(err.Error(), "conflicting digests") {
		t.Fatalf("conflicting digests error = %v", err)
	}
}

func TestLocateRetainedImageFailsClosedWithoutDescriptorCapability(t *testing.T) {
	deploy := &Deployer{config: &config.Config{}}
	located := false
	retained, err := locateRetainedImageGated([]string{"node-b", "node-a", "node-a"}, func(servers []string) error {
		if strings.Join(servers, ",") != "node-a,node-b" {
			t.Fatalf("capability check servers = %v", servers)
		}
		return deploy.ensureTakodCapability(fakeTakodStatusExecutor{
			output: `{"capabilities":["container.argv-v1"]}`,
		}, "node-a", takod.CapabilityImageDescriptorV1, "exact image reuse")
	}, func() ([]retainedImageSource, error) {
		located = true
		return nil, nil
	})
	var capabilityErr *takodclient.CapabilityRequiredError
	if retained || !errors.As(err, &capabilityErr) || capabilityErr.Capability != takod.CapabilityImageDescriptorV1 {
		t.Fatalf("retained = %v, err = %v; want CapabilityRequiredError", retained, err)
	}
	if located {
		t.Fatal("images were inspected before the capability gate passed")
	}

	retained, err = locateRetainedImageGated([]string{"node-a"}, func([]string) error { return nil }, func() ([]retainedImageSource, error) {
		return []retainedImageSource{{Platform: "linux/amd64", Server: "node-a", Descriptor: imageDescriptor("sha256:1")}}, nil
	})
	if err != nil || !retained {
		t.Fatalf("retained = %v, err = %v", retained, err)
	}
}
