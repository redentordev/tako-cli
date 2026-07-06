package engine

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/deployplan"
	"github.com/redentordev/tako-cli/pkg/git"
	"github.com/redentordev/tako-cli/pkg/takodstate"
)

// GitReader is the git metadata surface the engine needs from a repository.
type GitReader interface {
	IsRepository() bool
	HasUncommittedChanges() (bool, error)
	GetStatus() (string, error)
	GetCommitInfo(string) (*git.CommitInfo, error)
}

// SourceInfo describes the source identity of a deployment: either a git
// commit or an explicit source/revision label.
type SourceInfo struct {
	CommitInfo    *git.CommitInfo
	DirtyStatus   string
	BuildImageTag string
	StateSource   string
	SourceMode    bool
}

// GitStrings are the commit fields recorded with a deployment, empty-safe.
type GitStrings struct {
	Hash      string
	ShortHash string
	Branch    string
	Message   string
	Author    string
}

// GitStringsFromCommit flattens optional commit info into GitStrings.
func GitStringsFromCommit(commitInfo *git.CommitInfo) GitStrings {
	if commitInfo == nil {
		return GitStrings{}
	}
	return GitStrings{
		Hash:      commitInfo.Hash,
		ShortHash: commitInfo.ShortHash,
		Branch:    commitInfo.Branch,
		Message:   commitInfo.Message,
		Author:    commitInfo.Author,
	}
}

// GitInfoFromCommit converts optional commit info into takod state git info.
func GitInfoFromCommit(commitInfo *git.CommitInfo) takodstate.GitInfo {
	if commitInfo == nil {
		return takodstate.GitInfo{}
	}
	return takodstate.GitInfo{
		Commit:      commitInfo.Hash,
		CommitShort: commitInfo.ShortHash,
		Branch:      commitInfo.Branch,
		Message:     commitInfo.Message,
		Author:      commitInfo.Author,
	}
}

// ResolveSourceInfo determines deployment source identity. Default mode
// requires git; source mode skips git validation.
func ResolveSourceInfo(gitClient GitReader, allowDirty bool, source string, revision string, imageRef string, now time.Time) (SourceInfo, error) {
	source = strings.TrimSpace(source)
	revision = strings.TrimSpace(revision)
	imageRef = strings.TrimSpace(imageRef)
	if source != "" || revision != "" {
		var buildTag string
		var err error
		if imageRef != "" {
			buildTag, err = deployplan.ImageBuildTag(revision, imageRef)
		} else {
			buildTag, err = deployplan.SourceBuildTag(revision, now)
		}
		if err != nil {
			return SourceInfo{}, err
		}
		stateSource := source
		if stateSource == "" {
			stateSource = "source"
		}
		return SourceInfo{
			BuildImageTag: buildTag,
			StateSource:   stateSource,
			SourceMode:    true,
		}, nil
	}

	commitInfo, dirtyStatus, err := ResolveCommitInfo(gitClient, allowDirty)
	if err != nil {
		return SourceInfo{}, err
	}
	return SourceInfo{
		CommitInfo:    commitInfo,
		DirtyStatus:   dirtyStatus,
		BuildImageTag: commitInfo.Hash,
		StateSource:   "deploy",
	}, nil
}

// ResolveCommitInfo reads HEAD commit info, enforcing a clean worktree unless
// allowDirty is set.
func ResolveCommitInfo(gitClient GitReader, allowDirty bool) (*git.CommitInfo, string, error) {
	if !gitClient.IsRepository() {
		return nil, "", invalidRequestf("❌ This project is not a Git repository.\n\nPlease initialize Git first:\n  git init\n  git add .\n  git commit -m \"Initial commit\"\n\nGit is required for deployment tracking and rollback functionality.")
	}

	hasChanges, err := gitClient.HasUncommittedChanges()
	if err != nil {
		return nil, "", fmt.Errorf("failed to check git status: %w", err)
	}
	dirtyStatus := ""
	if hasChanges {
		status, err := gitClient.GetStatus()
		if err != nil {
			return nil, "", fmt.Errorf("failed to get git status: %w", err)
		}
		if strings.TrimSpace(status) == "" {
			status = "(dirty worktree)"
		}
		dirtyStatus = strings.TrimSpace(status)
		if !allowDirty {
			return nil, "", invalidRequestf("cannot deploy with uncommitted changes; commit, stash, or discard changes first:\n%s", dirtyStatus)
		}
	}

	commitInfo, err := gitClient.GetCommitInfo("")
	if err != nil {
		return nil, "", fmt.Errorf("failed to get commit info: %w", err)
	}
	return commitInfo, dirtyStatus, nil
}

// StartNotificationMessage builds the deploy-start notification text.
func StartNotificationMessage(project string, version string, envName string, revisionLabel string, revisionValue string, commitMessage string) string {
	message := fmt.Sprintf("Starting deployment of `%s` v%s to `%s`\n%s: `%s`", project, version, envName, revisionLabel, revisionValue)
	if commitMessage != "" {
		message += " - " + commitMessage
	}
	return message
}
