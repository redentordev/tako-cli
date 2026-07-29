package engine

import (
	"errors"
	"testing"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

type preflightSetupSpy struct {
	preflightErr  error
	gracefulErr   error
	preflights    int
	gracefulCalls int
	setups        int
}

func (s *preflightSetupSpy) PreflightGracefulStopCapabilities() error {
	s.gracefulCalls++
	return s.gracefulErr
}

func (s *preflightSetupSpy) PreflightTakodProxyCapabilities(map[string]config.ServiceConfig) error {
	s.preflights++
	return s.preflightErr
}

func (s *preflightSetupSpy) SetupTakodRuntime() error {
	s.setups++
	return nil
}

func TestRemoteProxyPreflightPreventsRuntimeSetupMutation(t *testing.T) {
	capabilityErr := &takodclient.CapabilityRequiredError{
		Server: "node-a", Capability: takod.CapabilityProxyRemoteMeshRoutesV1, Feature: "authoritative remote mesh proxy routes",
	}
	spy := &preflightSetupSpy{preflightErr: capabilityErr}
	err := preflightAndSetupTakodRuntime(spy, map[string]config.ServiceConfig{"web": {}})
	var got *takodclient.CapabilityRequiredError
	if !errors.As(err, &got) || got.Capability != takod.CapabilityProxyRemoteMeshRoutesV1 {
		t.Fatalf("preflight error = %v", err)
	}
	if spy.preflights != 1 || spy.gracefulCalls != 0 || spy.setups != 0 {
		t.Fatalf("proxy/graceful/setup calls = %d/%d/%d, want 1/0/0", spy.preflights, spy.gracefulCalls, spy.setups)
	}

	spy = &preflightSetupSpy{gracefulErr: errors.New("graceful stop unsupported")}
	if err := preflightAndSetupTakodRuntime(spy, nil); err == nil {
		t.Fatal("expected graceful-stop preflight failure")
	}
	if spy.preflights != 1 || spy.gracefulCalls != 1 || spy.setups != 0 {
		t.Fatalf("proxy/graceful/setup calls = %d/%d/%d, want 1/1/0", spy.preflights, spy.gracefulCalls, spy.setups)
	}

	spy = &preflightSetupSpy{}
	if err := preflightAndSetupTakodRuntime(spy, nil); err != nil {
		t.Fatal(err)
	}
	if spy.preflights != 1 || spy.gracefulCalls != 1 || spy.setups != 1 {
		t.Fatalf("successful proxy/graceful/setup calls = %d/%d/%d, want 1/1/1", spy.preflights, spy.gracefulCalls, spy.setups)
	}
}
