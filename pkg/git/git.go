package git

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var (
	gitCommandContext = exec.CommandContext
	gitCommandTimeout = 15 * time.Second
)

// Client handles Git operations
type Client struct {
	workDir string
}

// NewClient creates a new Git client
func NewClient(workDir string) *Client {
	if workDir == "" {
		workDir = "."
	}
	return &Client{workDir: workDir}
}

// IsRepository checks if the current directory is a Git repository
func (c *Client) IsRepository() bool {
	err := c.runGit("rev-parse", "--git-dir")
	return err == nil
}

// HasUncommittedChanges checks if there are uncommitted changes
func (c *Client) HasUncommittedChanges() (bool, error) {
	output, err := c.gitOutput("status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("failed to check git status: %w", err)
	}
	return len(strings.TrimSpace(string(output))) > 0, nil
}

// GetCurrentCommit returns the current commit hash
func (c *Client) GetCurrentCommit() (string, error) {
	output, err := c.gitOutput("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get current commit: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetShortCommit returns the short commit hash (7 chars)
func (c *Client) GetShortCommit() (string, error) {
	output, err := c.gitOutput("rev-parse", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get short commit: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetCommitMessage returns the commit message for a given hash
func (c *Client) GetCommitMessage(commitHash string) (string, error) {
	output, err := c.gitOutput("log", "-1", "--pretty=%B", commitHash)
	if err != nil {
		return "", fmt.Errorf("failed to get commit message: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetCurrentBranch returns the current branch name
func (c *Client) GetCurrentBranch() (string, error) {
	output, err := c.gitOutput("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetUserName returns the git user name
func (c *Client) GetUserName() (string, error) {
	output, err := c.gitOutput("config", "user.name")
	if err != nil {
		return "", fmt.Errorf("failed to get git user.name: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetUserEmail returns the git user email
func (c *Client) GetUserEmail() (string, error) {
	output, err := c.gitOutput("config", "user.email")
	if err != nil {
		return "", fmt.Errorf("failed to get git user.email: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetStatus returns the git status output
func (c *Client) GetStatus() (string, error) {
	output, err := c.gitOutput("status", "--short")
	if err != nil {
		return "", fmt.Errorf("failed to get git status: %w", err)
	}
	return string(output), nil
}

func (c *Client) runGit(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()

	cmd := gitCommandContext(ctx, "git", args...)
	cmd.Dir = c.workDir
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("git command timed out after %s: git %s", gitCommandTimeout, strings.Join(args, " "))
	}
	return err
}

func (c *Client) gitOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitCommandTimeout)
	defer cancel()

	cmd := gitCommandContext(ctx, "git", args...)
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("git command timed out after %s: git %s", gitCommandTimeout, strings.Join(args, " "))
	}
	return output, err
}

// GetCommitInfo returns formatted commit information
type CommitInfo struct {
	Hash      string
	ShortHash string
	Message   string
	Author    string
	Branch    string
}

// GetCommitInfo returns detailed commit information
func (c *Client) GetCommitInfo(commitHash string) (*CommitInfo, error) {
	if commitHash == "" {
		var err error
		commitHash, err = c.GetCurrentCommit()
		if err != nil {
			return nil, err
		}
	}

	shortHash, err := c.GetShortCommit()
	if err != nil {
		return nil, err
	}

	message, err := c.GetCommitMessage(commitHash)
	if err != nil {
		return nil, err
	}

	author, err := c.GetUserName()
	if err != nil {
		author = "unknown"
	}

	branch, err := c.GetCurrentBranch()
	if err != nil {
		branch = "unknown"
	}

	return &CommitInfo{
		Hash:      commitHash,
		ShortHash: shortHash,
		Message:   message,
		Author:    author,
		Branch:    branch,
	}, nil
}
