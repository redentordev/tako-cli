package cmd

import (
	"fmt"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
)

type remoteLeaseManager = engine.RemoteLeaseManager

// remoteOperationLeaseSet wraps the engine lease set to keep the historical
// Release(verbose) call shape used across commands.
type remoteOperationLeaseSet struct {
	*engine.RemoteLeaseSet
}

var acquireRemoteOperationLeasesFunc = acquireRemoteOperationLeases

func acquireRemoteOperationLeases(pool *ssh.Pool, cfg *config.Config, envName string, serverNames []string, operation string) (*remoteOperationLeaseSet, error) {
	set, err := engine.AcquireRemoteOperationLeases(pool, cfg, envName, serverNames, operation)
	if err != nil {
		return nil, err
	}
	return &remoteOperationLeaseSet{RemoteLeaseSet: set}, nil
}

func (s *remoteOperationLeaseSet) Release(verbose bool) {
	if s == nil || s.RemoteLeaseSet == nil {
		return
	}
	if verbose {
		s.SetWarnFunc(func(message string) {
			fmt.Print(message)
		})
	} else {
		s.SetWarnFunc(nil)
	}
	s.RemoteLeaseSet.Release()
}
