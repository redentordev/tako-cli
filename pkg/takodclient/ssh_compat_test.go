package takodclient_test

import (
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Keep the existing SSH direct-streamlocal client compatible with the
// structured AgentClient while consumers migrate away from shell/curl calls.
var _ takodclient.UnixSocketDialer = (*ssh.Client)(nil)
