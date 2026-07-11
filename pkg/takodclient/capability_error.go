package takodclient

import "fmt"

// CapabilityRequiredError rejects an operation before mutation when a target
// node agent does not advertise a capability required by the request.
type CapabilityRequiredError struct {
	Server     string
	Capability string
	Feature    string
}

func (e *CapabilityRequiredError) Error() string {
	return fmt.Sprintf("takod on %s does not support %s; run 'tako upgrade servers' (development builds can set TAKO_TAKOD_BINARY to a matching Linux binary)", e.Server, e.Feature)
}
