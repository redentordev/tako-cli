package takoapi

import (
	"strings"
	"time"
)

const (
	// APIVersionV1Alpha1 is the first additive canonical schema version.
	APIVersionV1Alpha1 = "tako.redentor.dev/v1alpha1"

	// APIVersionCurrent points at the current canonical schema version for new documents.
	APIVersionCurrent = APIVersionV1Alpha1

	// KindDeploymentDocument identifies a canonical desired deployment document.
	KindDeploymentDocument = "Deployment"

	// KindDeploymentService identifies a service entry within a deployment document.
	KindDeploymentService = "DeploymentService"
)

// SourceKind describes the operator input that produced a deployment revision.
type SourceKind string

const (
	SourceKindGit       SourceKind = "git"
	SourceKindDirectory SourceKind = "directory"
	SourceKindArchive   SourceKind = "archive"
	SourceKindImage     SourceKind = "image"
)

// IsValid reports whether k is one of Tako's canonical source kinds.
func (k SourceKind) IsValid() bool {
	switch k {
	case SourceKindGit, SourceKindDirectory, SourceKindArchive, SourceKindImage:
		return true
	default:
		return false
	}
}

// SourceIdentity identifies the deployment input independently from git metadata
// and independently from the durable deployment revision ID.
type SourceIdentity struct {
	Kind   SourceKind `json:"kind,omitempty"`
	Ref    string     `json:"ref,omitempty"`
	Digest string     `json:"digest,omitempty"`
}

// Normalize returns a copy with surrounding whitespace removed from string fields.
func (s SourceIdentity) Normalize() SourceIdentity {
	s.Kind = SourceKind(strings.TrimSpace(string(s.Kind)))
	s.Ref = strings.TrimSpace(s.Ref)
	s.Digest = strings.TrimSpace(s.Digest)
	return s
}

// IsZero reports whether no source identity was supplied.
func (s SourceIdentity) IsZero() bool {
	s = s.Normalize()
	return s.Kind == "" && s.Ref == "" && s.Digest == ""
}

// IsValid reports whether the source identity is structurally usable.
func (s SourceIdentity) IsValid() bool {
	s = s.Normalize()
	return s.Kind.IsValid() && (s.Ref != "" || s.Digest != "")
}

// RevisionIdentity identifies a whole deployment revision accepted by Tako. It
// must be independent from optional git metadata.
type RevisionIdentity struct {
	ID           string `json:"id,omitempty"`
	SourceDigest string `json:"sourceDigest,omitempty"`
	ConfigDigest string `json:"configDigest,omitempty"`
}

func (r RevisionIdentity) Normalize() RevisionIdentity {
	r.ID = strings.TrimSpace(r.ID)
	r.SourceDigest = strings.TrimSpace(r.SourceDigest)
	r.ConfigDigest = strings.TrimSpace(r.ConfigDigest)
	return r
}

func (r RevisionIdentity) IsZero() bool {
	r = r.Normalize()
	return r.ID == "" && r.SourceDigest == "" && r.ConfigDigest == ""
}

func (r RevisionIdentity) IsValid() bool {
	r = r.Normalize()
	return r.ID != ""
}

// ServiceRevisionIdentity identifies a per-service/container revision. It is
// distinct from RevisionIdentity, which identifies the whole deployment.
type ServiceRevisionIdentity struct {
	ID           string `json:"id,omitempty"`
	Service      string `json:"service,omitempty"`
	ConfigDigest string `json:"configDigest,omitempty"`
}

func (r ServiceRevisionIdentity) Normalize() ServiceRevisionIdentity {
	r.ID = strings.TrimSpace(r.ID)
	r.Service = strings.TrimSpace(r.Service)
	r.ConfigDigest = strings.TrimSpace(r.ConfigDigest)
	return r
}

func (r ServiceRevisionIdentity) IsZero() bool {
	r = r.Normalize()
	return r.ID == "" && r.Service == "" && r.ConfigDigest == ""
}

func (r ServiceRevisionIdentity) IsValid() bool {
	r = r.Normalize()
	return r.ID != "" && r.Service != ""
}

// ImageIdentity identifies a runtime artifact independently from the source and
// deployment revision that selected it.
type ImageIdentity struct {
	Ref    string `json:"ref,omitempty"`
	ID     string `json:"id,omitempty"`
	Digest string `json:"digest,omitempty"`
}

func (i ImageIdentity) Normalize() ImageIdentity {
	i.Ref = strings.TrimSpace(i.Ref)
	i.ID = strings.TrimSpace(i.ID)
	i.Digest = strings.TrimSpace(i.Digest)
	return i
}

func (i ImageIdentity) IsZero() bool {
	i = i.Normalize()
	return i.Ref == "" && i.ID == "" && i.Digest == ""
}

func (i ImageIdentity) IsValid() bool {
	return !i.IsZero()
}

// GitMetadata is optional display and trace metadata for git-backed sources.
// It is not a deployment revision identity and may be omitted for directory,
// archive, image, CI, or other non-git deployments.
type GitMetadata struct {
	Commit      string `json:"commit,omitempty"`
	CommitShort string `json:"commitShort,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
}

func (g GitMetadata) Normalize() GitMetadata {
	g.Commit = strings.TrimSpace(g.Commit)
	g.CommitShort = strings.TrimSpace(g.CommitShort)
	g.Branch = strings.TrimSpace(g.Branch)
	g.Message = strings.TrimSpace(g.Message)
	g.Author = strings.TrimSpace(g.Author)
	return g
}

func (g GitMetadata) IsZero() bool {
	g = g.Normalize()
	return g.Commit == "" && g.CommitShort == "" && g.Branch == "" && g.Message == "" && g.Author == ""
}

// HasCommit reports whether git commit metadata is present. It intentionally
// does not imply that the deployment revision ID is a git commit.
func (g GitMetadata) HasCommit() bool {
	g = g.Normalize()
	return g.Commit != "" || g.CommitShort != ""
}

// DeploymentDocument is the minimal canonical desired deployment document shape.
type DeploymentDocument struct {
	APIVersion  string                       `json:"apiVersion"`
	Kind        string                       `json:"kind"`
	Project     string                       `json:"project"`
	Environment string                       `json:"environment,omitempty"`
	Revision    RevisionIdentity             `json:"revision"`
	Source      SourceIdentity               `json:"source,omitempty"`
	Git         *GitMetadata                 `json:"git,omitempty"`
	Services    map[string]DeploymentService `json:"services,omitempty"`
	CreatedAt   time.Time                    `json:"createdAt,omitempty"`
}

// DeploymentService is a canonical service entry within a DeploymentDocument.
type DeploymentService struct {
	Kind     string                  `json:"kind,omitempty"`
	Name     string                  `json:"name"`
	Revision ServiceRevisionIdentity `json:"revision,omitempty"`
	Image    ImageIdentity           `json:"image,omitempty"`
	Source   SourceIdentity          `json:"source,omitempty"`
}

// NewDeploymentDocument returns a deployment document initialized with the
// current version and kind constants.
func NewDeploymentDocument(project, environment string) DeploymentDocument {
	return DeploymentDocument{
		APIVersion:  APIVersionCurrent,
		Kind:        KindDeploymentDocument,
		Project:     strings.TrimSpace(project),
		Environment: strings.TrimSpace(environment),
		Services:    map[string]DeploymentService{},
	}
}
