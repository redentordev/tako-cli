package ssh

import (
	"os"
	"strings"
	"testing"
)

func TestRemoteCopyCommandsQuotePaths(t *testing.T) {
	path := "/tmp/tako path'; touch /tmp/pwned #.txt"

	tests := map[string]string{
		"mkdir": buildRemoteMkdirCommand(path),
		"scp":   buildRemoteSCPReceiveCommand(path),
		"read":  buildRemoteReadCommand(path),
	}

	for name, cmd := range tests {
		if want := `'/tmp/tako path'"'"'; touch /tmp/pwned #.txt'`; !strings.Contains(cmd, want) {
			t.Fatalf("%s command did not quote path safely:\n%s", name, cmd)
		}
	}
}

func TestBuildSCPHeaderRejectsNewlineFileName(t *testing.T) {
	if _, err := buildSCPHeader("app\nmalicious", os.FileMode(0600), 12); err == nil {
		t.Fatal("buildSCPHeader should reject newline file names")
	}
}

func TestBuildSCPHeaderUsesBasenameModeAndSize(t *testing.T) {
	header, err := buildSCPHeader("app.tar", os.FileMode(0644), 99)
	if err != nil {
		t.Fatalf("buildSCPHeader returned error: %v", err)
	}

	if header != "C0644 99 app.tar\n" {
		t.Fatalf("header = %q", header)
	}
}
