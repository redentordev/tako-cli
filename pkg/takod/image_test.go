package takod

import "testing"

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
