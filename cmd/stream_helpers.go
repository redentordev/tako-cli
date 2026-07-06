package cmd

import (
	"io"
	"sync"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

type lockedLogWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func connectTakodStreamNode(server config.ServerConfig) (*ssh.Client, error) {
	client, err := ssh.NewClientFromConfig(ssh.ServerConfig{
		Host:     server.Host,
		Port:     server.Port,
		User:     server.User,
		SSHKey:   server.SSHKey,
		Password: server.Password,
	})
	if err != nil {
		return nil, err
	}
	if err := client.Connect(); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}
