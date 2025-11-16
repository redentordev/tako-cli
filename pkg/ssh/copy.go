package ssh

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
		mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteDir)
		if _, err := c.Execute(mkdirCmd); err != nil {
			return fmt.Errorf("failed to create remote directory: %w", err)
		}
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
		fmt.Fprintf(stdin, "C0%o %d %s\n", fileInfo.Mode().Perm(), fileInfo.Size(), filepath.Base(remotePath))

		// Send file content
		io.Copy(stdin, localFile)

		// Send null byte to indicate end
		fmt.Fprint(stdin, "\x00")
	}()

	// Run SCP command
	cmd := fmt.Sprintf("scp -t %s", remotePath)
	if err := session.Run(cmd); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	return nil
}

// CopyDirectory copies a directory from local to remote server
func (c *Client) CopyDirectory(localPath, remotePath string) error {
	// Create remote directory
	mkdirCmd := fmt.Sprintf("mkdir -p %s", remotePath)
	if _, err := c.Execute(mkdirCmd); err != nil {
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
			mkdirCmd := fmt.Sprintf("mkdir -p %s", remoteFile)
			if _, err := c.Execute(mkdirCmd); err != nil {
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
	output, err := c.Execute(fmt.Sprintf("cat %s", remotePath))
	if err != nil {
		return fmt.Errorf("failed to read remote file: %w", err)
	}

	// Write to local file
	if err := os.WriteFile(localPath, []byte(output), 0644); err != nil {
		return fmt.Errorf("failed to write local file: %w", err)
	}

	return nil
}
