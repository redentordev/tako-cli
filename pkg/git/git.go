package git

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	cmd := exec.Command("git", "rev-parse", "--git-dir")
	cmd.Dir = c.workDir
	err := cmd.Run()
	return err == nil
}

// HasUncommittedChanges checks if there are uncommitted changes
func (c *Client) HasUncommittedChanges() (bool, error) {
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return false, fmt.Errorf("failed to check git status: %w", err)
	}
	return len(strings.TrimSpace(string(output))) > 0, nil
}

// GetCurrentCommit returns the current commit hash
func (c *Client) GetCurrentCommit() (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get current commit: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetShortCommit returns the short commit hash (7 chars)
func (c *Client) GetShortCommit() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get short commit: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetCommitMessage returns the commit message for a given hash
func (c *Client) GetCommitMessage(commitHash string) (string, error) {
	cmd := exec.Command("git", "log", "-1", "--pretty=%B", commitHash)
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get commit message: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetCurrentBranch returns the current branch name
func (c *Client) GetCurrentBranch() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get current branch: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetUserName returns the git user name
func (c *Client) GetUserName() (string, error) {
	cmd := exec.Command("git", "config", "user.name")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git user.name: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetUserEmail returns the git user email
func (c *Client) GetUserEmail() (string, error) {
	cmd := exec.Command("git", "config", "user.email")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git user.email: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// GetStatus returns the git status output
func (c *Client) GetStatus() (string, error) {
	cmd := exec.Command("git", "status", "--short")
	cmd.Dir = c.workDir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("failed to get git status: %w", err)
	}
	return string(output), nil
}

// AddAll stages all changes
func (c *Client) AddAll() error {
	cmd := exec.Command("git", "add", ".")
	cmd.Dir = c.workDir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to stage changes: %w", err)
	}
	return nil
}

// Commit creates a commit with the given message
func (c *Client) Commit(message string) error {
	cmd := exec.Command("git", "commit", "-m", message)
	cmd.Dir = c.workDir
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to create commit: %w", err)
	}
	return nil
}

// PromptCommitMessage prompts the user for a commit message
func PromptCommitMessage() (string, error) {
	fmt.Println("\nYou have uncommitted changes. Please enter a commit message:")
	fmt.Print("> ")

	reader := bufio.NewReader(os.Stdin)
	message, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("failed to read input: %w", err)
	}

	message = strings.TrimSpace(message)
	if message == "" {
		return "", fmt.Errorf("commit message cannot be empty")
	}

	return message, nil
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
