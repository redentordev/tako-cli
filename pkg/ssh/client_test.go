package ssh

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
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

func TestLockedBufferAllowsConcurrentWrites(t *testing.T) {
	var buffer lockedBuffer
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := buffer.Write([]byte("x")); err != nil {
				t.Errorf("Write returned error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := buffer.String(); len(got) != 50 || strings.Count(got, "x") != 50 {
		t.Fatalf("buffer = %q, want 50 x bytes", got)
	}
}

func TestConnectBackoffCapsDelay(t *testing.T) {
	tests := map[int]time.Duration{
		0: time.Second,
		1: time.Second,
		2: 2 * time.Second,
		4: 8 * time.Second,
		5: connectBackoffMax,
		8: connectBackoffMax,
	}

	for attempt, want := range tests {
		if got := connectBackoff(attempt); got != want {
			t.Fatalf("connectBackoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestConnectTimeoutUsesEnvironmentOverride(t *testing.T) {
	t.Setenv("TAKO_SSH_CONNECT_TIMEOUT", "7s")
	if got := connectTimeout(); got != 7*time.Second {
		t.Fatalf("connectTimeout = %s, want 7s", got)
	}

	t.Setenv("TAKO_SSH_CONNECT_TIMEOUT", "8")
	if got := connectTimeout(); got != 8*time.Second {
		t.Fatalf("connectTimeout seconds shorthand = %s, want 8s", got)
	}
}

func TestConnectTimeoutFallsBackAndCapsUnsafeValues(t *testing.T) {
	t.Setenv("TAKO_SSH_CONNECT_TIMEOUT", "bad")
	if got := connectTimeout(); got != connectDefaultTimeout {
		t.Fatalf("invalid connectTimeout = %s, want default %s", got, connectDefaultTimeout)
	}

	t.Setenv("TAKO_SSH_CONNECT_TIMEOUT", "500ms")
	if got := connectTimeout(); got != connectDefaultTimeout {
		t.Fatalf("subsecond connectTimeout = %s, want default %s", got, connectDefaultTimeout)
	}

	t.Setenv("TAKO_SSH_CONNECT_TIMEOUT", "10m")
	if got := connectTimeout(); got != connectMaxTimeout {
		t.Fatalf("oversized connectTimeout = %s, want cap %s", got, connectMaxTimeout)
	}
}

func TestConnectTCPAttemptsUsesEnvironmentOverride(t *testing.T) {
	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "1")
	if got := connectTCPAttempts(); got != 1 {
		t.Fatalf("connectTCPAttempts = %d, want 1", got)
	}

	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "bad")
	if got := connectTCPAttempts(); got != connectDefaultTCPAttempts {
		t.Fatalf("invalid connectTCPAttempts = %d, want default %d", got, connectDefaultTCPAttempts)
	}

	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "100")
	if got := connectTCPAttempts(); got != 20 {
		t.Fatalf("oversized connectTCPAttempts = %d, want cap 20", got)
	}
}

func TestConnectTCPAttemptsUsesAutomationDefault(t *testing.T) {
	t.Setenv("CI", "true")
	if got := connectTCPAttempts(); got != connectAutomationTCPAttempts {
		t.Fatalf("CI connectTCPAttempts = %d, want %d", got, connectAutomationTCPAttempts)
	}

	t.Setenv("CI", "")
	t.Setenv("TAKO_NONINTERACTIVE", "1")
	if got := connectTCPAttempts(); got != connectAutomationTCPAttempts {
		t.Fatalf("noninteractive connectTCPAttempts = %d, want %d", got, connectAutomationTCPAttempts)
	}
}

func TestConnectTCPAttemptsOverrideWinsInAutomation(t *testing.T) {
	t.Setenv("CI", "true")
	t.Setenv("TAKO_SSH_CONNECT_ATTEMPTS", "2")
	if got := connectTCPAttempts(); got != 2 {
		t.Fatalf("override connectTCPAttempts = %d, want 2", got)
	}
}

func TestIsTransientDialErrorClassifiesRateLimitFailures(t *testing.T) {
	refused := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}
	if !isTransientDialError(refused) {
		t.Fatal("connection refused should be treated as transient")
	}
	if !isTransientDialError(os.ErrDeadlineExceeded) {
		t.Fatal("deadline exceeded should be treated as transient")
	}
	if isTransientDialError(os.ErrNotExist) {
		t.Fatal("unrelated local errors should not be treated as transient")
	}
}
