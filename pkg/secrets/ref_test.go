package secrets

import "testing"

func TestSecretRefKey(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{ref: "KEY", want: "KEY"},
		{ref: "VAR:KEY", want: "KEY"},
		{ref: "VAR:KEY:EXTRA", want: "KEY:EXTRA"},
		{ref: "", want: ""},
	}
	for _, tt := range tests {
		if got := SecretRefKey(tt.ref); got != tt.want {
			t.Errorf("SecretRefKey(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}
