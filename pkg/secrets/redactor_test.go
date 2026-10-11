package secrets

import (
	"strings"
	"testing"
)

func TestRedactorPublicValuesSurviveShortSecretCollisions(t *testing.T) {
	r := NewRedactor()
	r.Register("site")
	r.RegisterPublic("site-renderer")

	got := r.Redact("Deploying service: site-renderer (mode site)")
	if !strings.Contains(got, "site-renderer") {
		t.Fatalf("public service name was masked: %q", got)
	}
	if strings.Contains(got, "mode site") {
		t.Fatalf("registered secret outside a public value must still be masked: %q", got)
	}
}

func TestRedactorPublicValueThatIsAlsoASecretStaysRedacted(t *testing.T) {
	r := NewRedactor()
	r.Register("hunter2-password")
	r.RegisterPublic("hunter2-password")

	if got := r.Redact("value hunter2-password"); strings.Contains(got, "hunter2-password") {
		t.Fatalf("a value registered as both secret and public must be masked: %q", got)
	}
}

func TestRedactorKeepsHexImageTagsAndDigests(t *testing.T) {
	r := NewRedactor()
	sha := "1b2615e0c3d4a5b6c7d8e9f00112233445566778"
	digest := strings.Repeat("ab", 32)
	for _, input := range []string{
		"Image changed: registry.example.com/cms/admin:" + sha,
		"pulled registry.example.com/cms/admin@sha256:" + digest,
	} {
		if got := r.Redact(input); got != input {
			t.Fatalf("image reference was masked:\n got  %q\n want %q", got, input)
		}
	}
}

func TestRedactorStillMasksHexSecrets(t *testing.T) {
	r := NewRedactor()
	hex40 := "0123456789abcdef0123456789abcdef01234567"
	hex64 := strings.Repeat("cd", 32)
	for _, input := range []string{
		"token: " + hex40,
		"HETZNER_TOKEN=" + hex64,
		"raw " + hex64,
		"raw " + hex40,
	} {
		got := r.Redact(input)
		if strings.Contains(got, hex40) || strings.Contains(got, hex64) {
			t.Fatalf("hex secret leaked: %q -> %q", input, got)
		}
	}
}

func TestShouldRedactEnvValue(t *testing.T) {
	cases := []struct {
		name, value string
		want        bool
	}{
		{"CMS_RUNTIME_MODE", "site", false},
		{"NODE_ENV", "production", false},
		{"PORT", "3000", false},
		{"WORKFLOW_REDIS_URL", "redis://redis:6379", false},
		{"SITE_URL", "https://site.example.com/path", false},
		{"GENERIC_TIMEZONE", "Asia/Manila", false},
		{"API_TOKEN", "short", true},
		{"BETTER_AUTH_SECRET", "anything", true},
		{"DATABASE_URL", "postgres://app:pa55word@db:5432/app", true},
		{"SLACK_HOOK_URL", "https://hooks.slack.com/services/T0000/B0000/abcd1234efgh5678ijkl", true},
		{"STRIPE", "sk_test_51HabcdEFGH1234567890", true},
		{"MESSAGE", "hello world 1234567890 abc", false},
		{"EMPTY", "", false},
	}
	for _, tc := range cases {
		if got := ShouldRedactEnvValue(tc.name, tc.value); got != tc.want {
			t.Errorf("ShouldRedactEnvValue(%q, %q) = %v, want %v", tc.name, tc.value, got, tc.want)
		}
	}
}
