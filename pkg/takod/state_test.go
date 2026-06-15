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
		Content:     "{\"project\":\"demo-app\",\"environment\":\"production\",\"revisionId\":\"20260613T120000Z_abc123\"}\n",
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

func TestWriteAndReadNodeActualStateDocument(t *testing.T) {
	dataDir := t.TempDir()
	request := StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActualNode,
		Node:        "node-a",
		Content:     "{\"project\":\"demo\",\"environment\":\"production\",\"node\":\"node-a\"}\n",
	}

	if _, err := WriteStateDocument(context.Background(), dataDir, request); err != nil {
		t.Fatalf("WriteStateDocument returned error: %v", err)
	}

	read, err := ReadStateDocument(context.Background(), dataDir, StateDocumentRequest{
		Project:     request.Project,
		Environment: request.Environment,
		Document:    request.Document,
		Node:        request.Node,
	})
	if err != nil {
		t.Fatalf("ReadStateDocument returned error: %v", err)
	}
	if !read.Found || read.Content != request.Content {
		t.Fatalf("unexpected node actual response: %#v", read)
	}

	expectedPath := filepath.Join(dataDir, "actual", request.Project, request.Environment, "nodes", request.Node+".json")
	if read.Path != expectedPath {
		t.Fatalf("node actual path = %q, want %q", read.Path, expectedPath)
	}
}

func TestNodeActualStateDocumentRequiresSafeNode(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActualNode,
		Node:        "../node",
		Content:     "{}",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid node") {
		t.Fatalf("expected invalid node error, got %v", err)
	}
}

func TestWriteAndReadHistoryStateDocument(t *testing.T) {
	dataDir := t.TempDir()
	request := StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentHistory,
		Content:     "{\"projectName\":\"demo\",\"environment\":\"production\",\"deployments\":[]}\n",
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

func TestWriteStateDocumentRejectsMalformedContent(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		Content:     "{",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid desired state document JSON") {
		t.Fatalf("expected malformed JSON error, got %v", err)
	}
}

func TestWriteStateDocumentRejectsMultipleJSONValues(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		Content:     `{"ok":true} {"extra":true}`,
	})
	if err == nil || !strings.Contains(err.Error(), "single JSON value") {
		t.Fatalf("expected single JSON value error, got %v", err)
	}
}

func TestWriteStateDocumentRejectsNonObjectContent(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActual,
		Content:     `[]`,
	})
	if err == nil || !strings.Contains(err.Error(), "must be a JSON object") {
		t.Fatalf("expected JSON object error, got %v", err)
	}
}

func TestWriteStateDocumentRejectsProjectMismatch(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		RevisionID:  "rev_1",
		Content:     `{"project":"other","environment":"production","revisionId":"rev_1"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "project mismatch") {
		t.Fatalf("expected project mismatch error, got %v", err)
	}
}

func TestWriteStateDocumentRejectsNodeMismatch(t *testing.T) {
	_, err := WriteStateDocument(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentActualNode,
		Node:        "node-a",
		Content:     `{"project":"demo","environment":"production","node":"node-b"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "node mismatch") {
		t.Fatalf("expected node mismatch error, got %v", err)
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
		Content:     "{\"type\":\"deploy\",\"project\":\"demo\",\"environment\":\"staging\"}\n\n",
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
	if got, want := string(data), "{\"type\":\"deploy\",\"project\":\"demo\",\"environment\":\"staging\"}\n"; got != want {
		t.Fatalf("event log = %q, want %q", got, want)
	}
}

func TestAppendStateEventRejectsMalformedContent(t *testing.T) {
	_, err := AppendStateEvent(context.Background(), t.TempDir(), StateDocumentRequest{
		Project:     "demo",
		Environment: "staging",
		Content:     "deploy",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid event state document JSON") {
		t.Fatalf("expected event JSON error, got %v", err)
	}
}

func TestStateDocumentValidationRejectsUnsafeNames(t *testing.T) {
	valid := StateDocumentRequest{
		Project:     "demo",
		Environment: "production",
		Document:    stateDocumentDesired,
		RevisionID:  "rev_123",
		Content:     `{"project":"demo","environment":"production","revisionId":"rev_123"}`,
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
		Content:     `{"project":"demo","environment":"production"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected append-only event error, got %v", err)
	}
}
