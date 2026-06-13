package takod

import (
	"path/filepath"
	"testing"
)

func TestValidateImageName(t *testing.T) {
	if err := validateImageName("demo/web:abc123"); err != nil {
		t.Fatalf("expected image name to be valid: %v", err)
	}
	for _, image := range []string{"", "demo\nweb:abc", "demo\rweb:abc", "demo\x00web:abc"} {
		if err := validateImageName(image); err == nil {
			t.Fatalf("expected image name %q to be rejected", image)
		}
	}
}

func TestSanitizeImageArchiveName(t *testing.T) {
	got := sanitizeImageArchiveName("registry.example.com/demo/web:abc123")
	want := "registry.example.com-demo-web-abc123"
	if got != want {
		t.Fatalf("sanitizeImageArchiveName() = %q, want %q", got, want)
	}
}

func TestSafeArchiveTargetRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	valid, err := safeArchiveTarget(root, "app/Dockerfile")
	if err != nil {
		t.Fatalf("expected valid archive path: %v", err)
	}
	if filepath.Dir(valid) != filepath.Join(root, "app") {
		t.Fatalf("unexpected valid target: %s", valid)
	}

	for _, name := range []string{"../Dockerfile", "/etc/passwd", "app/../../secret"} {
		if _, err := safeArchiveTarget(root, name); err == nil {
			t.Fatalf("expected archive path %q to be rejected", name)
		}
	}
}
