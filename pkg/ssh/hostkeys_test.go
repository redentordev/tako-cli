package ssh

import (
	"crypto/ed25519"
	"os"
	"strings"
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

func TestNewHostKeyVerifierRegistersInteractivePrompt(t *testing.T) {
	verifier, err := NewHostKeyVerifier(HostKeyModeAsk)
	if err != nil {
		t.Fatalf("NewHostKeyVerifier returned error: %v", err)
	}
	if verifier.promptFn == nil {
		t.Fatal("NewHostKeyVerifier should register the default interactive prompt")
	}
}

func TestHostKeyAskWithoutPromptDoesNotFallbackToTOFU(t *testing.T) {
	sshPublic := testHostKeyPublicKey(t)
	knownHostsPath := t.TempDir() + "/known_hosts"
	verifier := &HostKeyVerifier{
		mode:          HostKeyModeAsk,
		takoHostsPath: knownHostsPath,
		promptFn:      nil,
	}

	err := verifier.GetCallback()("node.example:22", nil, sshPublic)
	if err == nil {
		t.Fatal("ask mode without a prompt should fail closed")
	}
	if !strings.Contains(err.Error(), "requires an interactive prompt") {
		t.Fatalf("error = %q, want interactive prompt guidance", err)
	}
	if _, statErr := os.Stat(knownHostsPath); !os.IsNotExist(statErr) {
		t.Fatalf("known_hosts was written despite failed ask prompt, statErr=%v", statErr)
	}
}

func TestInteractivePromptRejectsNonTerminal(t *testing.T) {
	_, err := InteractivePrompt("node.example", "SHA256:test", "ssh-ed25519")
	if err == nil {
		t.Fatal("InteractivePrompt should reject non-terminal stdin")
	}
	if !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("error = %q, want terminal guidance", err)
	}
}

func TestHostKeyVerifierConcurrentModeAndVerify(t *testing.T) {
	sshPublic := testHostKeyPublicKey(t)

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

func testHostKeyPublicKey(t *testing.T) gossh.PublicKey {
	t.Helper()
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("GenerateKey returned error: %v", err)
	}

	sshPublic, err := gossh.NewPublicKey(public)
	if err != nil {
		t.Fatalf("NewPublicKey returned error: %v", err)
	}
	return sshPublic
}
