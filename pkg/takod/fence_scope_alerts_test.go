package takod

import (
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

func TestAlertConfigRequestIsBoundToOperationFenceScope(t *testing.T) {
	fence := nodeidentity.OperationFence{Project: "demo", Environment: "production"}
	request := AlertConfigRequest{Project: "demo", Environment: "production"}
	if err := validateEndpointPayloadScope(&request, fence); err != nil {
		t.Fatal(err)
	}
	request.Environment = "staging"
	if err := validateEndpointPayloadScope(&request, fence); err == nil {
		t.Fatal("out-of-scope alert request was accepted")
	}
}
