package ssh

import (
	"os"
	"testing"
)

func TestBuildRemoteUploadCommandQuotesRemotePath(t *testing.T) {
	cmd := buildRemoteUploadCommand("/tmp/tako path'; touch /tmp/pwned #.json", 0600)
	want := `base64 -d > '/tmp/tako path'"'"'; touch /tmp/pwned #.json' && chmod 600 '/tmp/tako path'"'"'; touch /tmp/pwned #.json'`

	if cmd != want {
		t.Fatalf("unexpected upload command:\nwant: %s\n got: %s", want, cmd)
	}
}

func TestBuildRemoteUploadCommandHandlesEmptyPath(t *testing.T) {
	cmd := buildRemoteUploadCommand("", os.FileMode(0640))

	if cmd != "base64 -d > '' && chmod 640 ''" {
		t.Fatalf("unexpected upload command: %s", cmd)
	}
}
