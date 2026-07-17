package takoapi

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestStateDocumentConstructorsInitializeCanonicalShape(t *testing.T) {
	desired := NewDesiredStateDocument(" demo ", " production ", " rev-1 ")
	if desired.APIVersion != APIVersionCurrent || desired.Kind != KindDesiredStateDocument || desired.SchemaVersion != StateSchemaVersionCurrent {
		t.Fatalf("desired version/kind/schema mismatch: %#v", desired)
	}
	if desired.Project != "demo" || desired.Environment != "production" || desired.RevisionID != "rev-1" {
		t.Fatalf("desired identity not normalized: %#v", desired)
	}
	if desired.Services == nil {
		t.Fatal("desired Services map was not initialized")
	}

	actual := NewActualStateDocument(" demo ", " staging ")
	if actual.APIVersion != APIVersionCurrent || actual.Kind != KindActualStateDocument || actual.SchemaVersion != StateSchemaVersionCurrent {
		t.Fatalf("actual version/kind/schema mismatch: %#v", actual)
	}
	if actual.Project != "demo" || actual.Environment != "staging" || actual.Services == nil || actual.Nodes == nil {
		t.Fatalf("actual constructor mismatch: %#v", actual)
	}

	node := NewActualNodeStateDocument(" demo ", " staging ", " node-a ")
	if node.APIVersion != APIVersionCurrent || node.Kind != KindActualNodeStateDocument || node.SchemaVersion != StateSchemaVersionCurrent {
		t.Fatalf("node actual version/kind/schema mismatch: %#v", node)
	}
	if node.Project != "demo" || node.Environment != "staging" || node.Node != "node-a" || node.Services == nil {
		t.Fatalf("node actual constructor mismatch: %#v", node)
	}

	event := NewStateEventDocument(" demo ", " staging ", " deploy.started ")
	if event.APIVersion != APIVersionCurrent || event.Kind != KindStateEventDocument || event.SchemaVersion != StateSchemaVersionCurrent {
		t.Fatalf("event version/kind/schema mismatch: %#v", event)
	}
	if event.Project != "demo" || event.Environment != "staging" || event.Type != "deploy.started" || event.Details == nil {
		t.Fatalf("event constructor mismatch: %#v", event)
	}
}

func TestDesiredStateDocumentJSONIdentityShape(t *testing.T) {
	doc := NewDesiredStateDocument("demo", "production", "rev-1")
	doc.Source = "git:https://example.com/acme/demo.git#main"
	doc.TargetNodes = []string{"node-a"}
	doc.Git = &GitMetadata{Commit: "abcdef123456", CommitShort: "abcdef1", Branch: "main"}
	doc.CreatedAt = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	doc.Services["web"] = DesiredServiceDocument{
		APIVersion:     APIVersionCurrent,
		Kind:           KindDesiredServiceDocument,
		WorkloadKind:   "run",
		Files:          []ServiceFileDocument{{Source: "./config", Target: "/etc/demo", Secret: true}},
		Name:           "web",
		Type:           "web",
		Image:          "ghcr.io/acme/web:1",
		Replicas:       2,
		Assignments:    []ReplicaAssignmentDocument{{Slot: 1, Node: "node-a", NodeID: "id-a"}},
		Placement:      json.RawMessage(`{"node":"node-a"}`),
		HealthCheck:    json.RawMessage(`{"path":"/health"}`),
		DeployStrategy: "rolling",
		RemovalPending: true,
	}

	got := mustMarshalMap(t, doc)
	assertJSONField(t, got, "apiVersion", APIVersionCurrent)
	assertJSONField(t, got, "kind", KindDesiredStateDocument)
	assertJSONField(t, got, "project", "demo")
	assertJSONField(t, got, "environment", "production")
	assertJSONField(t, got, "revisionId", "rev-1")
	assertJSONNumber(t, got, "schemaVersion", StateSchemaVersionCurrent)

	services, ok := got["services"].(map[string]any)
	if !ok {
		t.Fatalf("services shape = %#v", got["services"])
	}
	web, ok := services["web"].(map[string]any)
	if !ok {
		t.Fatalf("web service shape = %#v", services["web"])
	}
	if web["kind"] != KindDesiredServiceDocument || web["name"] != "web" {
		t.Fatalf("web service identity mismatch: %#v", web)
	}
	if web["workloadKind"] != "run" {
		t.Fatalf("web workload kind mismatch: %#v", web)
	}
	if web["removalPending"] != true {
		t.Fatalf("web removal-pending marker mismatch: %#v", web)
	}
	assignments, ok := web["assignments"].([]any)
	if !ok || len(assignments) != 1 {
		t.Fatalf("web assignments shape = %#v", web["assignments"])
	}
	assignment, ok := assignments[0].(map[string]any)
	if !ok || assignment["node"] != "node-a" || assignment["nodeId"] != "id-a" {
		t.Fatalf("web assignment identity mismatch: %#v", assignments[0])
	}
	files, ok := web["files"].([]any)
	if !ok || len(files) != 1 || files[0].(map[string]any)["target"] != "/etc/demo" {
		t.Fatalf("web files mismatch: %#v", web["files"])
	}
}

func TestActualStateDocumentJSONIdentityShape(t *testing.T) {
	doc := NewActualStateDocument("demo", "production")
	doc.TargetNodes = []string{"node-a"}
	doc.CapturedAt = time.Date(2026, 7, 5, 12, 1, 0, 0, time.UTC)
	doc.Services["web"] = ActualServiceDocument{
		APIVersion:       APIVersionCurrent,
		Kind:             KindActualServiceDocument,
		Name:             "web",
		Image:            "ghcr.io/acme/web:1",
		Replicas:         1,
		Containers:       []string{"container-a"},
		CurrentRevision:  "rev-1",
		DeployStrategy:   "rolling",
		ActiveContainers: []string{"container-a"},
	}

	got := mustMarshalMap(t, doc)
	assertJSONField(t, got, "apiVersion", APIVersionCurrent)
	assertJSONField(t, got, "kind", KindActualStateDocument)
	assertJSONField(t, got, "project", "demo")
	assertJSONField(t, got, "environment", "production")
	assertJSONNumber(t, got, "schemaVersion", StateSchemaVersionCurrent)

	services, ok := got["services"].(map[string]any)
	if !ok {
		t.Fatalf("services shape = %#v", got["services"])
	}
	web, ok := services["web"].(map[string]any)
	if !ok || web["kind"] != KindActualServiceDocument || web["name"] != "web" {
		t.Fatalf("web service shape mismatch: %#v", services["web"])
	}
}

func TestDeploymentHistoryDocumentsDecodeInternalStateJSON(t *testing.T) {
	input := `{
		"projectName":"demo",
		"environment":"production",
		"server":"node-a",
		"deployments":[{
			"id":"deploy_123",
			"timestamp":"2026-07-05T12:00:00Z",
			"projectName":"demo",
			"environment":"production",
			"version":"v1",
			"status":"success",
			"services":{"web":{"name":"web","image":"ghcr.io/acme/web:1","imageId":"sha256:abc","containerId":"container-a","port":8080,"replicas":2,"env":{"PORT":"8080"},"healthCheck":{"enabled":true,"path":"/health","healthy":true,"lastCheck":"2026-07-05T12:01:00Z"}}},
			"user":"alice",
			"host":"node-a",
			"duration":5000000000,
			"message":"deployed",
			"gitCommit":"abcdef123456",
			"gitCommitShort":"abcdef1",
			"gitBranch":"main",
			"gitCommitMsg":"ship it",
			"gitAuthor":"Alice",
			"cliVersion":"1.2.3",
			"cliCommit":"cliabcdef"
		}],
		"lastUpdated":"2026-07-05T12:02:00Z"
	}`

	var history DeploymentHistoryDocument
	if err := json.Unmarshal([]byte(input), &history); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if history.ProjectName != "demo" || history.Environment != "production" || len(history.Deployments) != 1 {
		t.Fatalf("history identity = %#v", history)
	}
	deployment := history.Deployments[0]
	if deployment.ID != "deploy_123" || deployment.Status != StatusSuccess || deployment.Duration != 5*time.Second {
		t.Fatalf("deployment fields = %#v", deployment)
	}
	service := deployment.Services["web"]
	if service.ImageID != "sha256:abc" || service.ContainerID != "container-a" || service.HealthCheck.Path != "/health" || !service.HealthCheck.Healthy {
		t.Fatalf("service fields = %#v", service)
	}

	got := mustMarshalMap(t, history)
	assertJSONField(t, got, "projectName", "demo")
	deployments, ok := got["deployments"].([]any)
	if !ok || len(deployments) != 1 {
		t.Fatalf("deployments shape = %#v", got["deployments"])
	}
	deploymentMap, ok := deployments[0].(map[string]any)
	if !ok {
		t.Fatalf("deployment shape = %#v", deployments[0])
	}
	assertJSONField(t, deploymentMap, "gitCommitShort", "abcdef1")
	if deploymentMap["duration"] != float64((5 * time.Second).Nanoseconds()) {
		t.Fatalf("duration = %v, want %d", deploymentMap["duration"], (5 * time.Second).Nanoseconds())
	}
}

func TestTakodAcceptsCanonicalStateDocuments(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()

	desired := NewDesiredStateDocument("demo", "production", "rev-1")
	desired.Services["web"] = DesiredServiceDocument{Name: "web", Type: "web", Replicas: 1}
	desiredContent := mustMarshalString(t, desired)
	if _, err := takod.WriteStateDocument(ctx, dataDir, takod.StateDocumentRequest{
		Project:     desired.Project,
		Environment: desired.Environment,
		Document:    StateDocumentDesired,
		RevisionID:  desired.RevisionID,
		Content:     desiredContent,
	}); err != nil {
		t.Fatalf("WriteStateDocument(desired) error = %v", err)
	}

	actual := NewActualStateDocument("demo", "production")
	actual.Services["web"] = ActualServiceDocument{Name: "web", Replicas: 1}
	actualContent := mustMarshalString(t, actual)
	if _, err := takod.WriteStateDocument(ctx, dataDir, takod.StateDocumentRequest{
		Project:     actual.Project,
		Environment: actual.Environment,
		Document:    StateDocumentActual,
		Content:     actualContent,
	}); err != nil {
		t.Fatalf("WriteStateDocument(actual) error = %v", err)
	}

	nodeActual := NewActualNodeStateDocument("demo", "production", "node-a")
	nodeActual.Services["web"] = ActualServiceDocument{Name: "web", Replicas: 1}
	nodeActualContent := mustMarshalString(t, nodeActual)
	if _, err := takod.WriteStateDocument(ctx, dataDir, takod.StateDocumentRequest{
		Project:     nodeActual.Project,
		Environment: nodeActual.Environment,
		Document:    StateDocumentActualNode,
		Node:        nodeActual.Node,
		Content:     nodeActualContent,
	}); err != nil {
		t.Fatalf("WriteStateDocument(actual-node) error = %v", err)
	}

	event := NewStateEventDocument("demo", "production", "deploy.completed")
	event.RevisionID = desired.RevisionID
	event.Message = "deployment completed"
	event.Time = time.Date(2026, 7, 5, 12, 2, 0, 0, time.UTC)
	eventContent := mustMarshalString(t, event)
	if _, err := takod.AppendStateEvent(ctx, dataDir, takod.StateDocumentRequest{
		Project:     event.Project,
		Environment: event.Environment,
		Document:    StateDocumentEvent,
		Content:     eventContent,
	}); err != nil {
		t.Fatalf("AppendStateEvent(event) error = %v", err)
	}

	deployment := DeploymentStateDocument{
		ID:          "deploy_1",
		Timestamp:   time.Date(2026, 7, 5, 12, 3, 0, 0, time.UTC),
		ProjectName: "demo",
		Environment: "production",
		Version:     "rev-1",
		Status:      StatusSuccess,
		Services:    map[string]ServiceStateDocument{"web": {Name: "web", Replicas: 1}},
		User:        "alice",
		Host:        "node-a",
		Duration:    time.Second,
		Message:     "deployment completed",
	}
	deploymentContent := mustMarshalString(t, deployment)
	if _, err := takod.WriteStateDocument(ctx, dataDir, takod.StateDocumentRequest{
		Project:     deployment.ProjectName,
		Environment: deployment.Environment,
		Document:    StateDocumentDeployment,
		RevisionID:  deployment.ID,
		Content:     deploymentContent,
	}); err != nil {
		t.Fatalf("WriteStateDocument(deployment) error = %v", err)
	}

	history := DeploymentHistoryDocument{
		ProjectName: "demo",
		Environment: "production",
		Server:      "node-a",
		Deployments: []*DeploymentStateDocument{&deployment},
		LastUpdated: time.Date(2026, 7, 5, 12, 4, 0, 0, time.UTC),
	}
	historyContent := mustMarshalString(t, history)
	if _, err := takod.WriteStateDocument(ctx, dataDir, takod.StateDocumentRequest{
		Project:     history.ProjectName,
		Environment: history.Environment,
		Document:    StateDocumentHistory,
		Content:     historyContent,
	}); err != nil {
		t.Fatalf("WriteStateDocument(history) error = %v", err)
	}
}

func mustMarshalMap(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	return got
}

func mustMarshalString(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(encoded)
}

func assertJSONField(t *testing.T, got map[string]any, key, want string) {
	t.Helper()
	if got[key] != want {
		t.Fatalf("%s = %v, want %s", key, got[key], want)
	}
}

func assertJSONNumber(t *testing.T, got map[string]any, key string, want int) {
	t.Helper()
	value, ok := got[key].(float64)
	if !ok || value != float64(want) {
		t.Fatalf("%s = %v, want %d", key, got[key], want)
	}
}
