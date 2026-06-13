package provisioner

import "testing"

func TestSystemdPathArg(t *testing.T) {
	path, err := systemdPathArg("", "/run/tako/takod.sock")
	if err != nil {
		t.Fatalf("systemdPathArg returned error: %v", err)
	}
	if path != "/run/tako/takod.sock" {
		t.Fatalf("unexpected fallback path %q", path)
	}

	for _, value := range []string{"relative/path", "/run/tako/bad path.sock", "/run/tako/bad\npath.sock"} {
		if _, err := systemdPathArg(value, ""); err == nil {
			t.Fatalf("expected %q to be rejected", value)
		}
	}
}
