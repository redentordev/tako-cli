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

func newLabeledRedactor(t *testing.T) *Redactor {
	t.Helper()
	t.Setenv(RedactionLabelsEnv, "")
	return NewRedactor()
}

func TestRedactorNoPartialReveal(t *testing.T) {
	for _, value := range []string{"abcde", "abcdefgh", "abcdefghijklmnopqrstuvwxyz0123"} {
		r := newLabeledRedactor(t)
		r.RegisterLabeled(value, EnvLabel("API_SECRET"))
		got := r.Redact("value " + value + " end")
		if got != "value [REDACTED:env.API_SECRET] end" {
			t.Fatalf("Redact(%q) = %q", value, got)
		}
		if strings.Contains(got, "***") {
			t.Fatalf("partial reveal in %q", got)
		}
	}
}

func TestRedactorLabelPrecedenceIndependentOfOrder(t *testing.T) {
	for _, order := range [][2]string{{"env.DB_PASS", "secrets.DB_PASSWORD"}, {"secrets.DB_PASSWORD", "env.DB_PASS"}} {
		r := newLabeledRedactor(t)
		r.RegisterLabeled("pa55word-db", order[0])
		r.RegisterLabeled("pa55word-db", order[1])
		if got := r.Redact("pw pa55word-db"); got != "pw [REDACTED:secrets.DB_PASSWORD]" {
			t.Fatalf("order %v: %q", order, got)
		}
	}
}

func TestRedactorUnlabeledRegisterRendersBareMarker(t *testing.T) {
	r := newLabeledRedactor(t)
	r.Register("unlabeled-value")
	if got := r.Redact("x unlabeled-value"); got != "x [REDACTED]" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorPatternLabels(t *testing.T) {
	r := newLabeledRedactor(t)
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	cases := []struct{ in, want string }{
		{"password=hunter2hunter2", "password=[REDACTED:pattern.assignment]"},
		{"token: abc123", "token: [REDACTED:pattern.assignment]"},
		{"auth " + jwt, "auth [REDACTED:pattern.assignment]"},
		{"got " + jwt, "got [REDACTED:pattern.jwt]"},
		{"dsn postgres://app:pa55@db/app", "dsn postgres://app:[REDACTED:pattern.connection-string]@db/app"},
		{"id AKIAABCDEFGHIJKLMNOP", "id [REDACTED:pattern.aws-access-key]"},
	}
	for _, tc := range cases {
		if got := r.Redact(tc.in); got != tc.want {
			t.Errorf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRedactorOverlappingSecretsDeterministic(t *testing.T) {
	var first string
	for i := 0; i < 100; i++ {
		r := newLabeledRedactor(t)
		r.RegisterLabeled("abcd1234", "env.A_TOKEN")
		r.RegisterLabeled("xxabcd1234yy", "env.B_TOKEN")
		got := r.Redact("v xxabcd1234yy and abcd1234")
		if got != "v [REDACTED:env.B_TOKEN] and [REDACTED:env.A_TOKEN]" {
			t.Fatalf("run %d: %q", i, got)
		}
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("nondeterministic output: %q vs %q", got, first)
		}
	}
}

func TestRedactorPartiallyOverlappingSecretsMaskUnion(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterLabeled("abcdXYZW", "env.A_TOKEN")
	r.RegisterLabeled("XYZWqrstuv", "env.B_TOKEN")
	got := r.Redact("[abcdXYZWqrstuv]")
	if got != "[[REDACTED:env.B_TOKEN]]" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorPublicNameDoesNotShieldLongerSecret(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterPublic("acme")
	r.RegisterLabeled("acme-prod-token-9f8e7d6c", SecretLabel("API_TOKEN"))
	got := r.Redact("project acme uses acme-prod-token-9f8e7d6c")
	if got != "project acme uses [REDACTED:secrets.API_TOKEN]" {
		t.Fatalf("got %q", got)
	}
	for _, fragment := range []string{"-prod-token-9f8e7d6c", "prod", "9f8e7d6c"} {
		if strings.Contains(got, fragment) {
			t.Fatalf("fragment %q leaked: %q", fragment, got)
		}
	}
}

func TestRedactorPublicOverlappingSecretEdgesDoesNotShield(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterPublic("web-frontend")
	r.RegisterLabeled("s3cr3t-web", "env.A_TOKEN")      // overlaps the public start
	r.RegisterLabeled("frontend-t0ken", "env.B_TOKEN") // overlaps the public end
	got := r.Redact("a s3cr3t-web-frontend-t0ken b")
	if strings.Contains(got, "s3cr3t") || strings.Contains(got, "t0ken") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, "[REDACTED:env.A_TOKEN]") {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorShortSecretInsideLongerPublicName(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterPublic("jardin-cms")
	r.RegisterLabeled("jardin", EnvLabel("SITE_TOKEN"))
	if got := r.Redact("deployed jardin-cms with jardin"); got != "deployed jardin-cms with [REDACTED:env.SITE_TOKEN]" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorIdempotent(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterLabeled("sk-live-registered-123", SecretLabel("API_KEY"))
	r.RegisterLabeled("REDACTED", EnvLabel("WEIRD_TOKEN"))
	r.RegisterPublic("site-renderer")
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"
	corpus := []string{
		"plain text with nothing",
		"key=sk-live-registered-123",
		"token: sk-live-registered-123, next",
		"Deploying site-renderer with sk-live-registered-123",
		"password=hunter2hunter2 and " + jwt,
		"value REDACTED inside",
		"[REDACTED] and [REDACTED:secrets.API_KEY] and [REDACTED:pattern.jwt]",
		"postgres://app:sk-live-registered-123@db/app",
		"[REDACTED:not-ours] and [REDACTED:sk-live-registered-123]",
	}
	for _, input := range corpus {
		once := r.Redact(input)
		if twice := r.Redact(once); twice != once {
			t.Errorf("not idempotent for %q:\n once  %q\n twice %q", input, once, twice)
		}
	}
}

func TestRedactorMarkerSurvivesAssignmentPattern(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterLabeled("some-api-key-value", SecretLabel("API_KEY"))
	in := "API_KEY=[REDACTED:secrets.API_KEY]"
	if got := r.Redact(in); got != in {
		t.Fatalf("got %q", got)
	}
	if got := r.Redact("API_KEY=some-api-key-value"); got != in {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorSecretEqualToMarkerWordDoesNotCorruptMarkers(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterLabeled("REDACTED", EnvLabel("WEIRD_TOKEN"))
	r.RegisterLabeled("another-secret-value", SecretLabel("OTHER"))
	got := r.Redact("REDACTED another-secret-value [REDACTED]")
	if got != "[REDACTED:env.WEIRD_TOKEN] [REDACTED:secrets.OTHER] [REDACTED]" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorMarkerShapedTextDoesNotShieldSecret(t *testing.T) {
	r := newLabeledRedactor(t)
	r.RegisterLabeled("ghp_abcdefghijklmnop1234", SecretLabel("GH_TOKEN"))
	got := r.Redact("[REDACTED:ghp_abcdefghijklmnop1234]")
	if got != "[REDACTED:[REDACTED:secrets.GH_TOKEN]]" {
		t.Fatalf("got %q", got)
	}
}

func TestRedactorLabelsOff(t *testing.T) {
	for _, value := range []string{"off", "0", "FALSE", "No"} {
		t.Setenv(RedactionLabelsEnv, value)
		r := NewRedactor()
		r.RegisterLabeled("stripe-secret-value", SecretLabel("STRIPE_KEY"))
		got := r.Redact("k stripe-secret-value password=hunter2hunter2")
		if got != "k [REDACTED] password=[REDACTED]" {
			t.Fatalf("%s: got %q", value, got)
		}
		if strings.Contains(got, "STRIPE_KEY") {
			t.Fatalf("%s: label leaked: %q", value, got)
		}
		if twice := r.Redact(got); twice != got {
			t.Fatalf("%s: not idempotent: %q", value, twice)
		}
	}
	t.Setenv(RedactionLabelsEnv, "on")
	if !NewRedactor().LabelsEnabled() {
		t.Fatalf("labels must stay on for other values")
	}
}