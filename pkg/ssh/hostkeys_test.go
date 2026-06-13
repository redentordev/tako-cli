package ssh

import "testing"

func TestParseHostKeyModeRejectsInsecureAliases(t *testing.T) {
	for _, input := range []string{"insecure", "none", "off", "bad"} {
		if _, err := ParseHostKeyMode(input); err == nil {
			t.Fatalf("expected host key mode %q to be rejected", input)
		}
	}
}

func TestParseHostKeyModeSupportedValues(t *testing.T) {
	tests := map[string]HostKeyMode{
		"":       HostKeyModeTOFU,
		"tofu":   HostKeyModeTOFU,
		"strict": HostKeyModeStrict,
		"ask":    HostKeyModeAsk,
	}

	for input, want := range tests {
		got, err := ParseHostKeyMode(input)
		if err != nil {
			t.Fatalf("ParseHostKeyMode(%q) returned error: %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseHostKeyMode(%q) = %v, want %v", input, got, want)
		}
	}
}
