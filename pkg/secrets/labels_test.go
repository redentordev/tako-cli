package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
)

func TestLabelConstructors(t *testing.T) {
	cases := []struct{ got, want string }{
		{SecretLabel("STRIPE_KEY"), "secrets.STRIPE_KEY"},
		{EnvLabel("API_TOKEN"), "env.API_TOKEN"},
		{EnvFileLabel("SMTP_PASSWORD"), "envFile.SMTP_PASSWORD"},
		{ServerPasswordLabel("app"), "servers.app.password"},
		{RegistryPasswordLabel("ghcr.io"), "registries.ghcr.io.password"},
		{RegistryTokenLabel("ghcr.io"), "registries.ghcr.io.token"},
		{ACMECredentialLabel("production", "CF_DNS_API_TOKEN"), "environments.production.proxy.acme.CF_DNS_API_TOKEN"},
		{BackupCredentialLabel("mongo", "secretAccessKey"), "services.mongo.backup.secretAccessKey"},
		{BuildArgLabel("web", "NPM_TOKEN"), "builds.web.args.NPM_TOKEN"},
		{NotificationLabel("slack"), "notifications.slack"},
		{HookPassEnvLabel("DEPLOY_TOKEN"), "hooks.passEnv.DEPLOY_TOKEN"},
		{RunEnvLabel("API_KEY"), "run.env.API_KEY"},
		{ExportPasswordLabel(), "export.password"},
		{CertsPrivateKeyLabel(), "certs.privateKey"},
		{ProxyPasswordLabel(), "proxy.password"},
		{PatternLabel("jwt"), "pattern.jwt"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("label = %q, want %q", tc.got, tc.want)
		}
		if !ValidLabel(tc.got) {
			t.Errorf("label %q does not match the marker grammar", tc.got)
		}
	}
}

func TestSanitizeLabel(t *testing.T) {
	cases := []struct{ in, want string }{
		{"env.API KEY", "env.API_KEY"},
		{"env.weird$name!", "env.weird_name_"},
		{"servers..password", "servers._.password"},
		{"env.ok-name@host/x:y", "env.ok-name@host/x:y"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := SanitizeLabel(tc.in); got != tc.want {
			t.Errorf("SanitizeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeLabelTruncatesKeepingKind(t *testing.T) {
	got := EnvLabel(strings.Repeat("A", 200))
	if len(got) != MaxLabelLength {
		t.Fatalf("len = %d, want %d", len(got), MaxLabelLength)
	}
	if !strings.HasPrefix(got, "env.") || !ValidLabel(got) {
		t.Fatalf("truncated label = %q", got)
	}
	// A truncation that would end on a separator drops it.
	label := SanitizeLabel("env." + strings.Repeat("B", MaxLabelLength-5) + ".tail")
	if strings.HasSuffix(label, ".") || !ValidLabel(label) {
		t.Fatalf("truncated label = %q", label)
	}
}

func TestLabelPrecedence(t *testing.T) {
	ordered := []string{"secrets.A", "envFile.A", "env.A", "registries.a.password", "servers.a.password", "environments.p.proxy.acme.X", "services.s.backup.accessKeyId", "builds.b.args.X", "notifications.slack", "hooks.passEnv.X", "run.env.X", "export.password", "certs.privateKey", "proxy.password", ""}
	for i := 0; i+1 < len(ordered); i++ {
		if !preferLabel(ordered[i], ordered[i+1]) || preferLabel(ordered[i+1], ordered[i]) {
			t.Errorf("%q should outrank %q", ordered[i], ordered[i+1])
		}
	}
	if !preferLabel("env.A", "env.B") {
		t.Errorf("ties must break lexicographically")
	}
}

func TestRedactionReasonStrings(t *testing.T) {
	for reason := ReasonSecretsList; reason <= ReasonEmpty; reason++ {
		if reason.String() == "" {
			t.Errorf("reason %d has no display string", reason)
		}
	}
	if ReasonPlainSetting.Masked() || ReasonEmpty.Masked() || !ReasonCredentialName.Masked() {
		t.Fatalf("Masked() mismatch")
	}
}

func TestClassifyEnvValueReasons(t *testing.T) {
	cases := []struct {
		name, value string
		want        RedactionReason
	}{
		{"CMS_RUNTIME_MODE", "site", ReasonPlainSetting},
		{"API_TOKEN", "abc", ReasonCredentialName},
		{"DATABASE_URL", "postgres://app:pa55word@db:5432/app", ReasonURLWithCredentials},
		{"SMTP_PASSWORD", "pa55w0rd-smtp", ReasonCredentialName},
		{"WEBHOOK_URL", "https://hooks.slack.com/services/T0000/B0000/abcd1234efgh5678ijkl", ReasonTokenURLPart},
		{"MONGODB_URI", "mongodb://mongo:27017/app", ReasonPlainSetting},
		{"STRIPE", "sk_test_51HabcdEFGH1234567890", ReasonTokenShapedValue},
		{"EMPTY", "", ReasonEmpty},

		// Locators under credential-like names stay visible unless they
		// carry credentials.
		{"BETTER_AUTH_URL", "https://admin.example.com", ReasonPlainSetting},
		{"BETTER_AUTH_TRUSTED_ORIGINS", "https://admin.example.com,https://site.example.com", ReasonPlainSetting},
		{"NEXTAUTH_URL", "https://app.example.com/api/auth", ReasonPlainSetting},
		{"KEYCLOAK_HOST", "sso.example.com", ReasonPlainSetting},
		{"OAUTH_ISSUER", "https://login.example.com/realms/main", ReasonPlainSetting},
		{"AUTH_URL", "https://user:pa55word@auth.example.com", ReasonURLWithCredentials},
		{"AUTH_CALLBACK_URL", "https://auth.example.com/cb/abcd1234efgh5678ijkl", ReasonTokenURLPart},
		{"AUTH_ORIGINS", "https://a.example.com,not a url", ReasonCredentialName},
		{"AUTH_HOST", "x9f3e2a7c41b8d6e05.example.com", ReasonTokenShapedValue},
		{"API_KEY_URL", "localhost", ReasonCredentialName},
		{"AUTH_SECRET", "https://admin.example.com", ReasonCredentialName},

		// Dotted host names are not token-shaped as a whole.
		{"N8N_HOST", "n8n1.vixilabs-example.com", ReasonPlainSetting},
		{"SITE_HOST", "site.example.com:8443", ReasonPlainSetting},
		{"MIXED", "Ab12.Cd34.Ef56.Gh78", ReasonTokenShapedValue},
	}
	for _, tc := range cases {
		if got := ClassifyEnvValue(tc.name, tc.value); got != tc.want {
			t.Errorf("ClassifyEnvValue(%q, %q) = %v, want %v", tc.name, tc.value, got, tc.want)
		}
	}
}

func TestLoadServiceEnvFilesPrecedence(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	first := write("first.env", "A=one\nB=one\n")
	second := write("second.env", "B=two\nC=two\n")
	single := write("single.env", "A=single\n")

	values, err := LoadServiceEnvFiles(&config.ServiceConfig{EnvFiles: []string{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	if values["A"] != "one" || values["B"] != "two" || values["C"] != "two" {
		t.Fatalf("later envFiles must override earlier ones: %#v", values)
	}

	values, err = LoadServiceEnvFiles(&config.ServiceConfig{EnvFile: single, EnvFiles: []string{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values["A"] != "single" {
		t.Fatalf("envFile must win over envFiles: %#v", values)
	}

	if _, err := LoadServiceEnvFiles(&config.ServiceConfig{EnvFile: filepath.Join(dir, "missing.env")}); err == nil {
		t.Fatalf("missing envFile must return an error")
	}
}
