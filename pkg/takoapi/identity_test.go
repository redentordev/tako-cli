package takoapi

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSourceIdentityNormalizeZeroAndValid(t *testing.T) {
	source := SourceIdentity{Kind: " git ", Ref: " https://example.com/repo.git#main ", Digest: " sha256:abc "}
	normalized := source.Normalize()

	if normalized.Kind != SourceKindGit || normalized.Ref != "https://example.com/repo.git#main" || normalized.Digest != "sha256:abc" {
		t.Fatalf("Normalize() = %#v", normalized)
	}
	if normalized.IsZero() {
		t.Fatal("normalized source should not be zero")
	}
	if !normalized.IsValid() {
		t.Fatal("normalized source should be valid")
	}

	if !(SourceIdentity{}).IsZero() {
		t.Fatal("empty source should be zero")
	}
	if (SourceIdentity{Kind: SourceKindGit}).IsValid() {
		t.Fatal("source with kind but no ref/digest should be invalid")
	}
	if (SourceIdentity{Kind: SourceKind("docker"), Ref: "nginx"}).IsValid() {
		t.Fatal("unknown source kind should be invalid")
	}
}

func TestRevisionIdentitiesAreSeparateAndValidate(t *testing.T) {
	revision := (RevisionIdentity{ID: " deploy-1 ", SourceDigest: " sha256:source ", ConfigDigest: " sha256:config "}).Normalize()
	if revision.ID != "deploy-1" || revision.SourceDigest != "sha256:source" || revision.ConfigDigest != "sha256:config" {
		t.Fatalf("RevisionIdentity Normalize() = %#v", revision)
	}
	if !revision.IsValid() || revision.IsZero() {
		t.Fatalf("RevisionIdentity validity/zero mismatch: %#v", revision)
	}
	if (RevisionIdentity{SourceDigest: "sha256:source"}).IsValid() {
		t.Fatal("whole deployment revision without ID should be invalid")
	}

	serviceRevision := (ServiceRevisionIdentity{ID: " svc-rev-1 ", Service: " web ", ConfigDigest: " sha256:svc "}).Normalize()
	if serviceRevision.ID != "svc-rev-1" || serviceRevision.Service != "web" || serviceRevision.ConfigDigest != "sha256:svc" {
		t.Fatalf("ServiceRevisionIdentity Normalize() = %#v", serviceRevision)
	}
	if !serviceRevision.IsValid() || serviceRevision.IsZero() {
		t.Fatalf("ServiceRevisionIdentity validity/zero mismatch: %#v", serviceRevision)
	}
	if (ServiceRevisionIdentity{ID: "svc-rev-1"}).IsValid() {
		t.Fatal("service revision without service name should be invalid")
	}
}

func TestImageIdentityNormalizeZeroAndValid(t *testing.T) {
	image := (ImageIdentity{Ref: " ghcr.io/acme/web:latest ", ID: " sha256:imageid ", Digest: " sha256:digest "}).Normalize()
	if image.Ref != "ghcr.io/acme/web:latest" || image.ID != "sha256:imageid" || image.Digest != "sha256:digest" {
		t.Fatalf("ImageIdentity Normalize() = %#v", image)
	}
	if !image.IsValid() || image.IsZero() {
		t.Fatalf("ImageIdentity validity/zero mismatch: %#v", image)
	}
	if (ImageIdentity{}).IsValid() {
		t.Fatal("empty image identity should be invalid")
	}
}

func TestGitMetadataIsOptionalAndDoesNotDriveRevision(t *testing.T) {
	withoutGit := NewDeploymentDocument("app", "production")
	withoutGit.Revision = RevisionIdentity{ID: "rev-directory-1"}
	withoutGit.Source = SourceIdentity{Kind: SourceKindDirectory, Ref: "."}

	encoded, err := json.Marshal(withoutGit)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if hasJSONKey(t, encoded, "git") {
		t.Fatalf("git should be omitted when nil: %s", encoded)
	}

	git := (GitMetadata{Commit: " abcdef123456 ", CommitShort: " abcdef1 ", Branch: " main ", Message: " deploy ", Author: " Ada "}).Normalize()
	if git.IsZero() || !git.HasCommit() {
		t.Fatalf("git helpers mismatch: %#v", git)
	}
	withGit := withoutGit
	withGit.Git = &git
	withGit.Revision = RevisionIdentity{ID: "rev-still-not-git-commit"}
	if withGit.Revision.ID == withGit.Git.Commit {
		t.Fatal("test setup invalid: revision should be independent from git commit")
	}

	shortOnlyGit := (GitMetadata{CommitShort: " abcdef1 "}).Normalize()
	if shortOnlyGit.IsZero() || !shortOnlyGit.HasCommit() {
		t.Fatalf("short-only git metadata helpers mismatch: %#v", shortOnlyGit)
	}

	emptyGit := (GitMetadata{Branch: " \t "}).Normalize()
	if !emptyGit.IsZero() || emptyGit.HasCommit() {
		t.Fatalf("empty git metadata helpers mismatch: %#v", emptyGit)
	}
}

func TestDeploymentDocumentJSONVersionKindShape(t *testing.T) {
	doc := NewDeploymentDocument(" app ", " production ")
	doc.CreatedAt = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	doc.Revision = RevisionIdentity{ID: "rev-1", SourceDigest: "sha256:source"}
	doc.Source = SourceIdentity{Kind: SourceKindImage, Ref: "ghcr.io/acme/web:1"}
	doc.Services["web"] = DeploymentService{
		Kind:     KindDeploymentService,
		Name:     "web",
		Revision: ServiceRevisionIdentity{ID: "svc-rev-1", Service: "web"},
		Image:    ImageIdentity{Ref: "ghcr.io/acme/web:1", Digest: "sha256:image"},
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got["apiVersion"] != APIVersionCurrent {
		t.Fatalf("apiVersion = %v, want %s", got["apiVersion"], APIVersionCurrent)
	}
	if got["kind"] != KindDeploymentDocument {
		t.Fatalf("kind = %v, want %s", got["kind"], KindDeploymentDocument)
	}
	if got["project"] != "app" || got["environment"] != "production" {
		t.Fatalf("project/environment not normalized in constructor: %v", got)
	}

	services, ok := got["services"].(map[string]any)
	if !ok {
		t.Fatalf("services shape = %#v", got["services"])
	}
	web, ok := services["web"].(map[string]any)
	if !ok {
		t.Fatalf("web service shape = %#v", services["web"])
	}
	if web["kind"] != KindDeploymentService {
		t.Fatalf("service kind = %v, want %s", web["kind"], KindDeploymentService)
	}
}

func hasJSONKey(t *testing.T, encoded []byte, key string) bool {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	_, ok := got[key]
	return ok
}
