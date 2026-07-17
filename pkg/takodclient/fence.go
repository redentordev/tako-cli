package takodclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

const (
	operationFenceHeader  = "X-Tako-Operation-Fence"
	operationHolderHeader = "X-Tako-Operation-Holder"
)

type operationFenceContextKey struct{}

// OperationFenceSource returns the latest renewed controller fence at request
// time. RemoteLeaseSet implements this so long operations do not keep sending
// the signature from their initial lease window.
type OperationFenceSource interface {
	OperationFence() *nodeidentity.OperationFence
}

type operationHolderSource interface {
	OperationHolderToken() string
}

// WithOperationFence binds the controller-signed mutation authority to every
// structured request issued with the returned context.
func WithOperationFence(ctx context.Context, fence *nodeidentity.OperationFence) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if fence == nil {
		return ctx
	}
	copy := *fence
	copy.TargetNodeIDs = append([]string(nil), fence.TargetNodeIDs...)
	return context.WithValue(ctx, operationFenceContextKey{}, copy)
}

func WithOperationFenceSource(ctx context.Context, source OperationFenceSource) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil {
		return ctx
	}
	return context.WithValue(ctx, operationFenceContextKey{}, source)
}

func withFallbackOperationFenceSource(ctx context.Context, source OperationFenceSource) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if source == nil || ctx.Value(operationFenceContextKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, operationFenceContextKey{}, source)
}

func attachOperationFenceHeader(request *http.Request) error {
	if request == nil {
		return nil
	}
	value := request.Context().Value(operationFenceContextKey{})
	var fence *nodeidentity.OperationFence
	switch typed := value.(type) {
	case nodeidentity.OperationFence:
		copy := typed
		fence = &copy
	case OperationFenceSource:
		fence = typed.OperationFence()
		if holderSource, ok := typed.(operationHolderSource); ok {
			if holderToken := holderSource.OperationHolderToken(); holderToken != "" {
				request.Header.Set(operationHolderHeader, holderToken)
			}
		}
	}
	if fence == nil {
		return nil
	}
	data, err := json.Marshal(fence)
	if err != nil {
		return fmt.Errorf("encode controller operation fence: %w", err)
	}
	request.Header.Set(operationFenceHeader, base64.RawURLEncoding.EncodeToString(data))
	return nil
}
