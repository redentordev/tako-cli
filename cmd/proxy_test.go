package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi"
	"golang.org/x/crypto/bcrypt"
)

func TestProxyHashPasswordFromStdin(t *testing.T) {
	restoreCost := proxyHashPasswordCost
	t.Cleanup(func() { proxyHashPasswordCost = restoreCost })
	proxyHashPasswordCost = bcrypt.MinCost

	var out bytes.Buffer
	cmd := proxyHashPasswordCmd
	cmd.SetIn(strings.NewReader("s3cret\n"))
	cmd.SetOut(&out)

	if err := runProxyHashPassword(cmd, nil); err != nil {
		t.Fatalf("runProxyHashPassword returned error: %v", err)
	}
	hash := strings.TrimSpace(out.String())
	if !strings.HasPrefix(hash, "$2a$") {
		t.Fatalf("output %q is not a bcrypt hash", hash)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte("s3cret")); err != nil {
		t.Fatalf("hash does not verify the password (trailing newline not stripped?): %v", err)
	}
}

func TestProxyHashPasswordCostBounds(t *testing.T) {
	restoreCost := proxyHashPasswordCost
	t.Cleanup(func() { proxyHashPasswordCost = restoreCost })

	proxyHashPasswordCost = bcrypt.MaxCost + 1
	err := runProxyHashPassword(proxyHashPasswordCmd, nil)
	if engine.Classify(err) != engine.ClassInvalid {
		t.Fatalf("out-of-range cost classified as %d, want ClassInvalid", engine.Classify(err))
	}
}

func TestReadProxyHashPasswordInputValidation(t *testing.T) {
	cases := []struct {
		name    string
		stdin   string
		wantErr string
	}{
		{"empty", "", "password is empty"},
		{"newline only", "\n", "password is empty"},
		{"too long", strings.Repeat("a", 73), "72 bytes"},
		{"crlf stripped", "pw\r\n", ""},
		{"inner spaces kept", " pw with spaces \n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := proxyHashPasswordCmd
			cmd.SetIn(strings.NewReader(tc.stdin))
			password, err := readProxyHashPasswordInput(cmd)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				want := strings.TrimSuffix(strings.TrimSuffix(tc.stdin, "\n"), "\r")
				if password != want {
					t.Fatalf("password = %q, want %q", password, want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestProxyHashPasswordResultGolden pins the machine-facing result schema; a
// failure here means a breaking (non-additive) change to the document.
func TestProxyHashPasswordResultGolden(t *testing.T) {
	result := engine.ProxyHashPasswordResult{
		APIVersion: takoapi.APIVersionCurrent,
		Kind:       engine.KindProxyHashPasswordResult,
		Cost:       10,
		Hash:       "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy",
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	want := `{
  "apiVersion": "tako.redentor.dev/v1alpha1",
  "kind": "ProxyHashPasswordResult",
  "cost": 10,
  "hash": "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"
}`
	if string(payload) != want {
		t.Fatalf("result document drifted:\n%s\nwant:\n%s", payload, want)
	}
}
