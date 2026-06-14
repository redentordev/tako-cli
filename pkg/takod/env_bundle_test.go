package takod

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteAndReadEnvBundle(t *testing.T) {
	dataDir := t.TempDir()
	content := base64.StdEncoding.EncodeToString([]byte("encrypted bytes"))
	updatedAt := time.Date(2026, 6, 13, 12, 30, 0, 0, time.UTC)
	request := EnvBundleRequest{
		Project:     "demo",
		Environment: "production",
		Content:     content,
		UpdatedAt:   updatedAt,
	}

	write, err := WriteEnvBundle(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("WriteEnvBundle returned error: %v", err)
	}
	if !write.Found {
		t.Fatal("expected write response to be found")
	}
	if write.UpdatedAt.IsZero() {
		t.Fatal("expected write response to include UpdatedAt")
	}
	if !write.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("write UpdatedAt = %s, want %s", write.UpdatedAt, updatedAt)
	}

	stored, err := os.ReadFile(filepath.Join(dataDir, "env", request.Project, request.Environment+".enc"))
	if err != nil {
		t.Fatalf("failed to read stored env bundle: %v", err)
	}
	var envelope envBundleEnvelope
	if err := json.Unmarshal(stored, &envelope); err != nil {
		t.Fatalf("failed to decode stored env bundle envelope: %v", err)
	}
	if envelope.Version != envBundleEnvelopeVersion {
		t.Fatalf("envelope version = %d, want %d", envelope.Version, envBundleEnvelopeVersion)
	}
	if envelope.Content != content {
		t.Fatalf("envelope content = %q, want %q", envelope.Content, content)
	}
	if !envelope.UpdatedAt.Equal(write.UpdatedAt) {
		t.Fatalf("envelope UpdatedAt = %s, want %s", envelope.UpdatedAt, write.UpdatedAt)
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
	if read.UpdatedAt.IsZero() {
		t.Fatal("expected read response to include UpdatedAt")
	}
}

func TestReadEnvBundleRejectsRawContent(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "env", "demo", "production.enc")
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		t.Fatalf("failed to create bundle directory: %v", err)
	}
	if err := os.WriteFile(path, []byte("raw encrypted bytes"), 0600); err != nil {
		t.Fatalf("failed to write raw bundle: %v", err)
	}

	_, err := ReadEnvBundle(context.Background(), dataDir, EnvBundleRequest{
		Project:     "demo",
		Environment: "production",
	})
	if err == nil {
		t.Fatal("expected raw bundle content to be rejected")
	}
	if !strings.Contains(err.Error(), "failed to decode environment bundle envelope") {
		t.Fatalf("error = %q, want envelope decode context", err)
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
