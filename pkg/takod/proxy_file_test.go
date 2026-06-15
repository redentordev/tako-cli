package takod

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndRemoveProxyFile(t *testing.T) {
	oldDir := proxyDynamicDir
	proxyDynamicDir = t.TempDir()
	t.Cleanup(func() { proxyDynamicDir = oldDir })

	response, err := WriteProxyFile(context.Background(), ProxyFileRequest{
		Name:    "demo-production.yml",
		Content: "http:\n  routers: {}\n",
	})
	if err != nil {
		t.Fatalf("WriteProxyFile returned error: %v", err)
	}
	wantPath := filepath.Join(proxyDynamicDir, "demo-production.yml")
	if response.Path != wantPath {
		t.Fatalf("path = %q, want %q", response.Path, wantPath)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("failed to read proxy file: %v", err)
	}
	if string(data) != "http:\n  routers: {}\n" {
		t.Fatalf("unexpected proxy file content: %q", string(data))
	}

	if _, err := RemoveProxyFile(context.Background(), "demo-production.yml"); err != nil {
		t.Fatalf("RemoveProxyFile returned error: %v", err)
	}
	if _, err := os.Stat(wantPath); !os.IsNotExist(err) {
		t.Fatalf("expected proxy file to be removed, stat err=%v", err)
	}
}

func TestValidateProxyFileNameRejectsUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"",
		"../demo.yml",
		"nested/demo.yml",
		`nested\demo.yml`,
		"demo.txt",
		"demo;rm.yml",
	} {
		if _, err := validateProxyFileName(name); err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
}
