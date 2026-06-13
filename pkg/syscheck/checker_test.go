package syscheck

import (
	"os"
	"testing"
)

func TestPromptNixpacksInstallReturnsFalseWhenNonInteractive(t *testing.T) {
	oldStdin := os.Stdin
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	_ = writer.Close()
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = reader.Close()
	})

	if NewSystemChecker(false).PromptNixpacksInstall() {
		t.Fatal("PromptNixpacksInstall should not install in non-interactive mode")
	}
}
