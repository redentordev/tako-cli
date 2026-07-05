package deployplan

import (
	"strings"
	"testing"
	"time"
)

func TestValidateBuildTagAcceptsValidExplicitRevision(t *testing.T) {
	valid := []string{
		"abc123",
		"_source",
		"A.B-c_123",
		strings.Repeat("a", 128),
	}

	for _, tag := range valid {
		t.Run(tag, func(t *testing.T) {
			if err := ValidateBuildTag(tag); err != nil {
				t.Fatalf("ValidateBuildTag(%q) returned error: %v", tag, err)
			}
		})
	}
}

func TestValidateBuildTagRejectsInvalidExplicitRevision(t *testing.T) {
	invalid := map[string]string{
		"":                       "empty",
		"bad/tag":                "invalid char",
		"bad:tag":                "invalid char",
		"-bad":                   "invalid first char",
		".bad":                   "invalid first char",
		strings.Repeat("a", 129): "too long",
	}

	for tag, name := range invalid {
		t.Run(name, func(t *testing.T) {
			if err := ValidateBuildTag(tag); err == nil {
				t.Fatalf("ValidateBuildTag(%q) returned nil error, want invalid", tag)
			}
		})
	}
}

func TestSourceBuildTagUsesExplicitRevision(t *testing.T) {
	got, err := SourceBuildTag("release_2026.07-05", time.Date(2026, 7, 5, 12, 34, 56, 0, time.FixedZone("UTC+8", 8*60*60)))
	if err != nil {
		t.Fatalf("SourceBuildTag() returned error: %v", err)
	}
	if got != "release_2026.07-05" {
		t.Fatalf("SourceBuildTag() = %q, want explicit revision unchanged", got)
	}
}

func TestSourceBuildTagRejectsInvalidExplicitRevision(t *testing.T) {
	if got, err := SourceBuildTag("bad/tag", time.Now()); err == nil {
		t.Fatalf("SourceBuildTag() = %q, nil error; want invalid revision error", got)
	}
}

func TestSourceBuildTagGeneratesUTCTimestampTag(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 34, 56, 0, time.FixedZone("UTC+8", 8*60*60))
	got, err := SourceBuildTag("", now)
	if err != nil {
		t.Fatalf("SourceBuildTag() returned error: %v", err)
	}
	const want = "source-20260705T043456Z"
	if got != want {
		t.Fatalf("SourceBuildTag() = %q, want %q", got, want)
	}
	if err := ValidateBuildTag(got); err != nil {
		t.Fatalf("generated tag is invalid: %v", err)
	}
}

func TestImageBuildTagDerivesDeterministicTag(t *testing.T) {
	got, err := ImageBuildTag("", " registry.example.com/web:sha \n")
	if err != nil {
		t.Fatalf("ImageBuildTag() returned error: %v", err)
	}
	const want = "image-8a5076f3bc4d"
	if got != want {
		t.Fatalf("ImageBuildTag() = %q, want %q", got, want)
	}
	if err := ValidateBuildTag(got); err != nil {
		t.Fatalf("generated tag is invalid: %v", err)
	}
}

func TestImageBuildTagUsesExplicitRevision(t *testing.T) {
	got, err := ImageBuildTag("release_2026.07-05", "registry.example.com/web:sha")
	if err != nil {
		t.Fatalf("ImageBuildTag() returned error: %v", err)
	}
	if got != "release_2026.07-05" {
		t.Fatalf("ImageBuildTag() = %q, want explicit revision unchanged", got)
	}
}

func TestImageBuildTagRejectsInvalidExplicitRevision(t *testing.T) {
	if got, err := ImageBuildTag("bad/tag", "registry.example.com/web:sha"); err == nil {
		t.Fatalf("ImageBuildTag() = %q, nil error; want invalid revision error", got)
	}
}

func TestImageBuildTagRejectsEmptyImageRefWhenDeriving(t *testing.T) {
	if got, err := ImageBuildTag("", " \t\n"); err == nil {
		t.Fatalf("ImageBuildTag() = %q, nil error; want empty image ref error", got)
	}
}
