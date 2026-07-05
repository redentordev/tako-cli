package deployer

import (
	"bytes"
	"io"
	"os"
	"testing"
)

func TestDeployerSetOutputRedirectsProgressOutput(t *testing.T) {
	var out bytes.Buffer
	deploy := &Deployer{}
	deploy.SetOutput(&out)

	deploy.printf("hello %s\n", "world")

	if got, want := out.String(), "hello world\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestDeployerSetOutputCanSilenceProgressOutput(t *testing.T) {
	deploy := &Deployer{}
	deploy.SetOutput(io.Discard)

	deploy.printf("hidden\n")

	if deploy.output != io.Discard {
		t.Fatalf("output writer = %#v, want io.Discard", deploy.output)
	}
}

func TestDeployerSetOutputNilResetsToStdout(t *testing.T) {
	var out bytes.Buffer
	deploy := &Deployer{}
	deploy.SetOutput(&out)
	deploy.SetOutput(nil)

	if deploy.output != os.Stdout {
		t.Fatalf("output writer = %#v, want os.Stdout", deploy.output)
	}
}

func TestStreamWriterWritesPrefixedLinesToInjectedWriter(t *testing.T) {
	var out bytes.Buffer
	writer := &streamWriter{prefix: "[node] ", writer: &out}

	if n, err := writer.Write([]byte("one\ntwo")); err != nil || n != len("one\ntwo") {
		t.Fatalf("first write returned n=%d err=%v", n, err)
	}
	if got, want := out.String(), "[node] one\n"; got != want {
		t.Fatalf("after first write output = %q, want %q", got, want)
	}

	if n, err := writer.Write([]byte(" continued\n")); err != nil || n != len(" continued\n") {
		t.Fatalf("second write returned n=%d err=%v", n, err)
	}
	if got, want := out.String(), "[node] one\n[node] two continued\n"; got != want {
		t.Fatalf("after second write output = %q, want %q", got, want)
	}
}
