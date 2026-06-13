package cmd

import "testing"

func TestPassphraseFromEnv(t *testing.T) {
	t.Setenv(envPassphraseVar, "correct horse battery staple")

	passphrase, ok, err := passphraseFromEnv()
	if err != nil {
		t.Fatalf("passphraseFromEnv returned error: %v", err)
	}
	if !ok {
		t.Fatal("passphraseFromEnv did not report an env passphrase")
	}
	if passphrase != "correct horse battery staple" {
		t.Fatalf("passphrase = %q", passphrase)
	}
}

func TestPassphraseFromEnvRejectsWeakSecret(t *testing.T) {
	t.Setenv(envPassphraseVar, "short")

	if _, ok, err := passphraseFromEnv(); !ok || err == nil {
		t.Fatalf("passphraseFromEnv ok=%v err=%v, want env detected with validation error", ok, err)
	}
}

func TestIsAllowedEnvBundlePath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: ".env", want: true},
		{path: ".tako/secrets", want: true},
		{path: ".tako/secrets.production", want: true},
		{path: ".tako/../.env", want: false},
		{path: "../.env", want: false},
		{path: "/tmp/.env", want: false},
		{path: ".tako/known_hosts", want: false},
		{path: "secrets.production", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isAllowedEnvBundlePath(tt.path); got != tt.want {
				t.Fatalf("isAllowedEnvBundlePath(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}
