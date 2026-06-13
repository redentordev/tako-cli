package takod

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteAndReadEnvBundle(t *testing.T) {
	dataDir := t.TempDir()
	content := base64.StdEncoding.EncodeToString([]byte("encrypted bytes"))
	request := EnvBundleRequest{
		Project:     "demo",
		Environment: "production",
		Content:     content,
	}

	write, err := WriteEnvBundle(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("WriteEnvBundle returned error: %v", err)
	}
	if !write.Found {
		t.Fatal("expected write response to be found")
	}

	stored, err := os.ReadFile(filepath.Join(dataDir, "env", request.Project, request.Environment+".enc"))
	if err != nil {
		t.Fatalf("failed to read stored env bundle: %v", err)
	}
	if string(stored) != "encrypted bytes" {
		t.Fatalf("stored env bundle = %q, want encrypted bytes", string(stored))
	}

	read, err := ReadEnvBundle(context.Background(), dataDir, EnvBundleRequest{
		Project:     request.Project,
		Environment: request.Environment,
	})
	if err != nil {
		t.Fatalf("ReadEnvBundle returned error: %v", err)
	}
	if !read.Found || read.Content != content {
		t.Fatalf("unexpected env bundle response: %#v", read)
	}
}

func TestReadEnvBundleReturnsNotFound(t *testing.T) {
	response, err := ReadEnvBundle(context.Background(), t.TempDir(), EnvBundleRequest{
		Project:     "demo",
		Environment: "production",
	})
	if err != nil {
		t.Fatalf("ReadEnvBundle returned error: %v", err)
	}
	if response.Found {
		t.Fatalf("expected missing env bundle to return Found=false: %#v", response)
	}
}

func TestWriteEnvBundleRejectsInvalidContent(t *testing.T) {
	_, err := WriteEnvBundle(context.Background(), t.TempDir(), EnvBundleRequest{
		Project:     "demo",
		Environment: "production",
		Content:     "not-base64%",
	})
	if err == nil {
		t.Fatal("expected invalid base64 content to fail")
	}
}

func TestEnvBundleValidationRejectsUnsafeNames(t *testing.T) {
	valid := EnvBundleRequest{
		Project:     "demo",
		Environment: "production",
		Content:     base64.StdEncoding.EncodeToString([]byte("encrypted")),
	}

	invalid := valid
	invalid.Project = "../demo"
	if err := validateEnvBundleRequest(invalid, true); err == nil {
		t.Fatal("expected unsafe project to be rejected")
	}

	invalid = valid
	invalid.Environment = "prod;rm"
	if err := validateEnvBundleRequest(invalid, true); err == nil {
		t.Fatal("expected unsafe environment to be rejected")
	}
}
