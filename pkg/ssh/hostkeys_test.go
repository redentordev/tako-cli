package ssh

import (
	"crypto/ed25519"
	"sync"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

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

func TestHostKeyVerifierConcurrentModeAndVerify(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}

	sshPublic, err := gossh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("NewPublicKey returned error: %v", err)
	}

	verifier := &HostKeyVerifier{
		mode:          HostKeyModeStrict,
		takoHostsPath: t.TempDir() + "/known_hosts",
		promptFn: func(_, _, _ string) (bool, error) {
			return false, nil
		},
	}
	callback := verifier.GetCallback()

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			verifier.SetMode(HostKeyModeStrict)
			verifier.SetMode(HostKeyModeAsk)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			verifier.SetPromptFunc(func(_, _, _ string) (bool, error) {
				return false, nil
			})
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = callback("node.example:22", nil, sshPublic)
		}
	}()

	wg.Wait()
}
