package setup

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestBuildWriteVersionFileCommandUsesEncodedPayload(t *testing.T) {
	payload := []byte("{\"version\":\"1.2.0\",\"note\":\"EOF && rm -rf /\"}")
	cmd := buildWriteVersionFileCommand(payload)

	if strings.Contains(cmd, string(payload)) {
		t.Fatalf("command contains raw JSON payload: %s", cmd)
	}
	if strings.Contains(cmd, "<<") {
		t.Fatalf("command should not use a heredoc: %s", cmd)
	}
	if !strings.Contains(cmd, shellQuote(base64.StdEncoding.EncodeToString(payload))) {
		t.Fatalf("command does not contain shell-quoted base64 payload: %s", cmd)
	}
	if !strings.Contains(cmd, "sudo install -d -m 0755 '/etc/tako'") {
		t.Fatalf("command does not create version directory safely: %s", cmd)
	}
	if !strings.Contains(cmd, "sudo install -m 0644 \"$tmp\" '/etc/tako/version.json'") {
		t.Fatalf("command does not install version file safely: %s", cmd)
	}
}

func TestSetupShellQuoteEscapesSingleQuotes(t *testing.T) {
	got := shellQuote("a'b")
	want := "'a'\"'\"'b'"
	if got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}
}
