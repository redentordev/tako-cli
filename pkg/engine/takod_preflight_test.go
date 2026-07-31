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
	serviceErr    error
	preflights    int
	gracefulCalls int
	serviceCalls  int
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

func (s *preflightSetupSpy) PreflightServiceMutationCapabilities(map[string]config.ServiceConfig) error {
	s.serviceCalls++
	return s.serviceErr
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
	err := preflightAndSetupTakodRuntime(spy, map[string]config.ServiceConfig{"web": {}}, nil)
	var got *takodclient.CapabilityRequiredError
	if !errors.As(err, &got) || got.Capability != takod.CapabilityProxyRemoteMeshRoutesV1 {
		t.Fatalf("preflight error = %v", err)
	}
	if spy.preflights != 1 || spy.gracefulCalls != 0 || spy.serviceCalls != 0 || spy.setups != 0 {
		t.Fatalf("proxy/graceful/service/setup calls = %d/%d/%d/%d, want 1/0/0/0", spy.preflights, spy.gracefulCalls, spy.serviceCalls, spy.setups)
	}

	spy = &preflightSetupSpy{gracefulErr: errors.New("graceful stop unsupported")}
	if err := preflightAndSetupTakodRuntime(spy, nil, nil); err == nil {
		t.Fatal("expected graceful-stop preflight failure")
	}
	if spy.preflights != 1 || spy.gracefulCalls != 1 || spy.serviceCalls != 0 || spy.setups != 0 {
		t.Fatalf("proxy/graceful/service/setup calls = %d/%d/%d/%d, want 1/1/0/0", spy.preflights, spy.gracefulCalls, spy.serviceCalls, spy.setups)
	}

	spy = &preflightSetupSpy{serviceErr: errors.New("service capability unsupported")}
	if err := preflightAndSetupTakodRuntime(spy, nil, map[string]config.ServiceConfig{"worker": {}}); err == nil {
		t.Fatal("expected service capability preflight failure")
	}
	if spy.preflights != 1 || spy.gracefulCalls != 1 || spy.serviceCalls != 1 || spy.setups != 0 {
		t.Fatalf("proxy/graceful/service/setup calls = %d/%d/%d/%d, want 1/1/1/0", spy.preflights, spy.gracefulCalls, spy.serviceCalls, spy.setups)
	}

	spy = &preflightSetupSpy{}
	if err := preflightAndSetupTakodRuntime(spy, nil, nil); err != nil {
		t.Fatal(err)
	}
	if spy.preflights != 1 || spy.gracefulCalls != 1 || spy.serviceCalls != 1 || spy.setups != 1 {
		t.Fatalf("successful proxy/graceful/service/setup calls = %d/%d/%d/%d, want 1/1/1/1", spy.preflights, spy.gracefulCalls, spy.serviceCalls, spy.setups)
	}
}
