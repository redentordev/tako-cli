package runtimeid

import (
	"strings"
	"testing"
)

// isDockerContainerNameSafe mirrors takod's isSafeContainerName.
func isDockerContainerNameSafe(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	first := name[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || (first >= '0' && first <= '9')) {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func TestRetainedContainerNameIsDeterministicAndDistinct(t *testing.T) {
	first := RetainedContainerName("demo", "production", "web", 1)
	if again := RetainedContainerName("demo", "production", "web", 1); again != first {
		t.Fatalf("RetainedContainerName not deterministic: %q != %q", first, again)
	}
	if !strings.Contains(first, "_prev_") {
		t.Fatalf("retained name %q missing prev component", first)
	}
	for _, other := range []string{
		ContainerName("demo", "production", "web", 1),
		RevisionContainerName("demo", "production", "web", "prev", 1),
		RetainedContainerName("demo", "production", "web", 2),
		RetainedContainerName("demo", "staging", "web", 1),
		// A service literally named web-prev must not collide either.
		ContainerName("demo", "production", "web-prev", 1),
		ContainerName("demo", "production", "web_prev", 1),
	} {
		if other == first {
			t.Fatalf("retained name %q collides with %q", first, other)
		}
	}
	if !isDockerContainerNameSafe(first) {
		t.Fatalf("retained name %q is not a safe container name", first)
	}
}

func TestRetainedContainerNameBoundsVeryLongInputs(t *testing.T) {
	project := strings.Repeat("p", 63)
	environment := strings.Repeat("e", 63)
	service := strings.Repeat("s", 63)
	for _, slot := range []int{0, 1, 99, 123456} {
		name := RetainedContainerName(project, environment, service, slot)
		if len(name) > 128 {
			t.Fatalf("retained name length = %d, want <= 128: %q", len(name), name)
		}
		if !isDockerContainerNameSafe(name) {
			t.Fatalf("retained name %q is not a safe container name", name)
		}
		if name == ContainerName(project, environment, service, slot) {
			t.Fatalf("retained name equals live slot name %q", name)
		}
	}
	if RetainedContainerName(project, environment, service, 1) == RetainedContainerName(project, environment, service, 2) {
		t.Fatal("truncated retained names must stay slot-distinct")
	}
}
