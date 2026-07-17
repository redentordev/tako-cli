package takodclient

import (
	"net/http"
	"testing"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

type testOperationAuthoritySource struct {
	fence  *nodeidentity.OperationFence
	holder string
}

func (s testOperationAuthoritySource) OperationFence() *nodeidentity.OperationFence { return s.fence }
func (s testOperationAuthoritySource) OperationHolderToken() string                 { return s.holder }

func TestAttachOperationFenceIncludesPrivateHolderCredential(t *testing.T) {
	source := testOperationAuthoritySource{fence: &nodeidentity.OperationFence{OperationID: "op-test"}, holder: "private-holder"}
	request, err := http.NewRequestWithContext(WithOperationFenceSource(t.Context(), source), http.MethodPost, "http://takod/v1/reconcile-service", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attachOperationFenceHeader(request); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get(operationFenceHeader) == "" || request.Header.Get(operationHolderHeader) != source.holder {
		t.Fatalf("operation authority headers = %#v", request.Header)
	}
}
