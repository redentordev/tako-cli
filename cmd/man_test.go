package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTrackedManPagesMatchGeneratedOutput(t *testing.T) {
	tempDir := t.TempDir()
	if err := GenerateManPages(tempDir); err != nil {
		t.Fatalf("failed to generate man pages: %v", err)
	}

	trackedDir := filepath.Join("..", "man")
	entries, err := os.ReadDir(trackedDir)
	if err != nil {
		t.Fatalf("failed to read tracked man pages: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("tracked man pages are missing; run `make man`")
	}

	trackedNames := make(map[string]bool)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".1" {
			continue
		}
		trackedNames[entry.Name()] = true
		trackedPath := filepath.Join(trackedDir, entry.Name())
		generatedPath := filepath.Join(tempDir, entry.Name())
		tracked, err := os.ReadFile(trackedPath)
		if err != nil {
			t.Fatalf("failed to read tracked man page %s: %v", entry.Name(), err)
		}
		generated, err := os.ReadFile(generatedPath)
		if err != nil {
			t.Fatalf("failed to read generated man page %s: %v", entry.Name(), err)
		}
		if string(tracked) != string(generated) {
			t.Fatalf("%s is stale; regenerate with `make man`", trackedPath)
		}
	}

	generatedEntries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatalf("failed to read generated man pages: %v", err)
	}
	for _, entry := range generatedEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".1" {
			continue
		}
		if !trackedNames[entry.Name()] {
			t.Fatalf("generated man page %s is not tracked; run `make man`", entry.Name())
		}
	}
}
