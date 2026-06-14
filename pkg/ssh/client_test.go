package ssh

import (
	"context"
	"io"
	"os"
	"testing"
	"time"
)

func TestWithDefaultCommandDeadlineAddsDeadlineWhenMissing(t *testing.T) {
	ctx, cancel, defaulted := withDefaultCommandDeadline(context.Background())
	defer cancel()

	if !defaulted {
		t.Fatal("withDefaultCommandDeadline should mark default deadline")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context should have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining <= DefaultCommandTimeout-5*time.Second || remaining > DefaultCommandTimeout {
		t.Fatalf("deadline remaining = %s, want close to %s", remaining, DefaultCommandTimeout)
	}
}

func TestWithDefaultCommandDeadlinePreservesExistingDeadline(t *testing.T) {
	parent, parentCancel := context.WithTimeout(context.Background(), time.Minute)
	defer parentCancel()

	ctx, cancel, defaulted := withDefaultCommandDeadline(parent)
	defer cancel()

	if defaulted {
		t.Fatal("withDefaultCommandDeadline should not replace existing deadline")
	}
	parentDeadline, _ := parent.Deadline()
	gotDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("context should keep parent deadline")
	}
	if !gotDeadline.Equal(parentDeadline) {
		t.Fatalf("deadline = %s, want parent deadline %s", gotDeadline, parentDeadline)
	}
}

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

func TestNewBase64ReaderStreamsEncodedContent(t *testing.T) {
	data, err := io.ReadAll(newBase64Reader([]byte("hello")))
	if err != nil {
		t.Fatalf("ReadAll returned error: %v", err)
	}
	if string(data) != "aGVsbG8=" {
		t.Fatalf("encoded content = %q, want aGVsbG8=", string(data))
	}
}
