package takod

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAndReadStateDocumentArchivesDesiredRevision(t *testing.T) {
	dataDir := t.TempDir()
	request := StateDocumentRequest{
		Project:     "demo-app",
		Environment: "production",
		Document:    stateDocumentDesired,
		RevisionID:  "20260613T120000Z_abc123",
		Content:     "{\"ok\":true}\n",
	}

	response, err := WriteStateDocument(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("WriteStateDocument returned error: %v", err)
	}
	if !response.Found {
		t.Fatal("expected written response to be found")
	}

	read, err := ReadStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     request.Project,
		Environment: request.Environment,
		Document:    request.Document,
	})
	if err != nil {
		t.Fatalf("ReadStateDocument returned error: %v", err)
	}
	if !read.Found || read.Content != request.Content {
		t.Fatalf("unexpected read response: %#v", read)
	}

	archivePath := filepath.Join(dataDir, "desired", request.Project, request.Environment, "revisions", request.RevisionID+".json")
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("failed to read archive: %v", err)
	}
	if string(archive) != request.Content {
		t.Fatalf("archive content = %q, want %q", string(archive), request.Content)
	}
}

func TestReadStateDocumentReturnsNotFound(t *testing.T) {
	response, err := ReadStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActual,
	})
	if err != nil {
		t.Fatalf("ReadStateDocument returned error: %v", err)
	}
	if response.Found {
		t.Fatalf("expected missing document to return Found=false: %#v", response)
	}
}

func TestWriteAndReadHistoryStateDocument(t *testing.T) {
	dataDir := t.TempDir()
	request := StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentHistory,
		Content:     "{\"deployments\":[]}\n",
	}
	if _, err := WriteStateDocument(context.Background(), dataDir, request); err != nil {
		t.Fatalf("WriteStateDocument returned error: %v", err)
	}
	read, err := ReadStateDocument(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("ReadStateDocument returned error: %v", err)
	}
	if !read.Found || read.Content != request.Content {
		t.Fatalf("unexpected history response: %#v", read)
	}
}

func TestDeploymentStateDocumentRequiresRevisionID(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDeployment,
		Content:     "{}",
	})
	if err == nil || !strings.Contains(err.Error(), "deployment ID") {
		t.Fatalf("expected deployment ID error, got %v", err)
	}
}

func TestAppendStateEventNormalizesNewline(t *testing.T) {
	dataDir := t.TempDir()
	request := StateDocumentRequest{
		Project:     "demo",
		Environment: "staging",
		Content:     "{\"type\":\"deploy\"}\n\n",
	}

	response, err := AppendStateEvent(context.Background(), dataDir, request)
	if err != nil {
		t.Fatalf("AppendStateEvent returned error: %v", err)
	}
	if !response.Found {
		t.Fatal("expected append response to be found")
	}

	data, err := os.ReadFile(filepath.Join(dataDir, "events", request.Project, request.Environment+".jsonl"))
	if err != nil {
		t.Fatalf("failed to read event log: %v", err)
	}
	if got, want := string(data), "{\"type\":\"deploy\"}\n"; got != want {
		t.Fatalf("event log = %q, want %q", got, want)
	}
}

func TestStateDocumentValidationRejectsUnsafeNames(t *testing.T) {
	valid := StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		RevisionID:  "rev_123",
		Content:     "{}",
	}

	for name, mutate := range map[string]func(*StateDocumentRequest){
		"project":     func(req *StateDocumentRequest) { req.Project = "../demo" },
		"environment": func(req *StateDocumentRequest) { req.Environment = "prod;rm" },
		"document":    func(req *StateDocumentRequest) { req.Document = "secrets" },
		"revision":    func(req *StateDocumentRequest) { req.RevisionID = "../rev" },
	} {
		t.Run(name, func(t *testing.T) {
			request := valid
			mutate(&request)
			if err := validateStateDocumentRequest(request, true); err == nil {
				t.Fatalf("expected %s validation to fail", name)
			}
		})
	}
}

func TestWriteStateDocumentRejectsEventOverwrite(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentEvent,
		Content:     "{}",
	})
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only event error, got %v", err)
	}
}
