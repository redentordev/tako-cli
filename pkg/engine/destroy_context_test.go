package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

func TestDestroySingleServerWithHooksContextStopsBeforePurgeAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	purgeCalled := false
	err := DestroySingleServerWithHooksContext(
		ctx,
		staticSSHClientProvider{client: &ssh.Client{}},
		"node-a",
		config.ServerConfig{Host: "node-a"},
		&config.Config{},
		"production",
		false,
		true,
		func(context.Context, *ssh.Client, *config.Config, string, bool) error {
			cancel()
			return nil
		},
		func(context.Context, *ssh.Client, *config.Config, string, bool) error {
			purgeCalled = true
			return nil
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if purgeCalled {
		t.Fatal("purge ran after the bound operation context was canceled")
	}
}

type staticSSHClientProvider struct {
	client *ssh.Client
}

func (p staticSSHClientProvider) GetOrCreateWithAuth(string, int, string, string, string) (*ssh.Client, error) {
	return p.client, nil
}
