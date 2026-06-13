package ssh

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/redentordev/tako-cli/pkg/fileutil"
)

// CopyFile copies a file from local to remote server
func (c *Client) CopyFile(localPath, remotePath string) error {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			return err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	// Open local file
	localFile, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer localFile.Close()

	// Get file info
	fileInfo, err := localFile.Stat()
	if err != nil {
		return fmt.Errorf("failed to stat local file: %w", err)
	}

	// Create remote directory if needed
	remoteDir := filepath.Dir(remotePath)
	if remoteDir != "." && remoteDir != "/" {
		if _, err := c.Execute(buildRemoteMkdirCommand(remoteDir)); err != nil {
			return fmt.Errorf("failed to create remote directory: %w", err)
		}
	}

	header, err := buildSCPHeader(filepath.Base(remotePath), fileInfo.Mode().Perm(), fileInfo.Size())
	if err != nil {
		return err
	}

	// Create SCP session
	session, err := c.conn.NewSession()
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	defer session.Close()

	// Set up stdin pipe
	stdin, err := session.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to get stdin pipe: %w", err)
	}

	// Start SCP command on remote
	go func() {
		defer stdin.Close()

		// Send file header
		_, _ = io.WriteString(stdin, header)

		// Send file content
		_, _ = io.Copy(stdin, localFile)

		// Send null byte to indicate end
		_, _ = io.WriteString(stdin, "\x00")
	}()

	// Run SCP command
	if err := session.Run(buildRemoteSCPReceiveCommand(remotePath)); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return nil
}

// CopyDirectory copies a directory from local to remote server
func (c *Client) CopyDirectory(localPath, remotePath string) error {
	// Create remote directory
	if _, err := c.Execute(buildRemoteMkdirCommand(remotePath)); err != nil {
		return fmt.Errorf("failed to create remote directory: %w", err)
	}

	// Walk through local directory
	return filepath.Walk(localPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Get relative path
		relPath, err := filepath.Rel(localPath, path)
		if err != nil {
			return err
		}

		// Construct remote path
		remoteFile := filepath.Join(remotePath, relPath)

		// Handle directories and files
		if info.IsDir() {
			if _, err := c.Execute(buildRemoteMkdirCommand(remoteFile)); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", remoteFile, err)
			}
		} else {
			if err := c.CopyFile(path, remoteFile); err != nil {
				return fmt.Errorf("failed to copy file %s: %w", path, err)
			}
		}

		return nil
	})
}

// CopyFromRemote copies a file from remote to local
func (c *Client) CopyFromRemote(remotePath, localPath string) error {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		if err := c.Connect(); err != nil {
			return err
		}
		c.mu.Lock()
	}
	c.mu.Unlock()

	// Create local directory if needed
	localDir := filepath.Dir(localPath)
	if err := os.MkdirAll(localDir, 0755); err != nil {
		return fmt.Errorf("failed to create local directory: %w", err)
	}

	// Get file content via cat
	output, err := c.Execute(buildRemoteReadCommand(remotePath))
	if err != nil {
		return fmt.Errorf("failed to read remote file: %w", err)
	}

	// Write to local file
	if err := fileutil.WriteFileAtomic(localPath, []byte(output), 0644); err != nil {
		return fmt.Errorf("failed to write local file: %w", err)
	}

	return nil
}

func buildRemoteMkdirCommand(remotePath string) string {
	return fmt.Sprintf("mkdir -p -- %s", shellQuote(remotePath))
}

func buildRemoteSCPReceiveCommand(remotePath string) string {
	return fmt.Sprintf("scp -t -- %s", shellQuote(remotePath))
}

func buildRemoteReadCommand(remotePath string) string {
	return fmt.Sprintf("cat -- %s", shellQuote(remotePath))
}

func buildSCPHeader(name string, mode os.FileMode, size int64) (string, error) {
	if name == "" || name == "." || name == "/" {
		return "", fmt.Errorf("invalid remote file name %q", name)
	}
	if strings.ContainsAny(name, "\r\n") {
		return "", fmt.Errorf("remote file name contains a newline")
	}
	return fmt.Sprintf("C0%o %d %s\n", mode.Perm(), size, name), nil
}
