package takod

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/nodeidentity"
)

// validateEndpointPayloadScope deliberately enumerates mutation payloads. A
// newly introduced request type fails closed instead of silently escaping
// project/environment fencing through reflection or an embedded document.
func validateEndpointPayloadScope(value any, fence nodeidentity.OperationFence) error {
	check := func(project, environment string) error {
		project, environment = strings.TrimSpace(project), strings.TrimSpace(environment)
		if project == "" || environment == "" {
			return fmt.Errorf("mutation payload has no unambiguous project/environment scope")
		}
		if project != fence.Project {
			return fmt.Errorf("request project is outside the controller operation fence")
		}
		if environment != fence.Environment {
			return fmt.Errorf("request environment is outside the controller operation fence")
		}
		return nil
	}
	switch req := value.(type) {
	case *ReconcileServiceRequest:
		return check(req.Project, req.Environment)
	case *RemoveServiceRequest:
		return check(req.Project, req.Environment)
	case *ServiceFilesRequest:
		return check(req.Project, req.Environment)
	case *ServiceFilesCheckRequest:
		return check(req.Project, req.Environment)
	case *CleanupRequest:
		return check(req.Project, req.Environment)
	case *StateDocumentRequest:
		return check(req.Project, req.Environment)
	case *EnvBundleRequest:
		return check(req.Project, req.Environment)
	case *BackupRequest:
		return check(req.Project, req.Environment)
	case *BackupScheduleRequest:
		return check(req.Project, req.Environment)
	case *ACMEDNSReconcileRequest:
		return check(req.Project, req.Environment)
	case *PortAllocationRequest:
		return check(req.Project, req.Environment)
	case *ExecRequest:
		return check(req.Project, req.Environment)
	case *JobsApplyRequest:
		if err := check(req.Project, req.Environment); err != nil {
			return err
		}
		for index := range req.Jobs {
			if err := check(req.Jobs[index].Project, req.Jobs[index].Environment); err != nil {
				return fmt.Errorf("job %d: %w", index, err)
			}
		}
		return nil
	case *JobTriggerRequest:
		return check(req.Project, req.Environment)
	case *ProxyFileRequest:
		manifest, err := ParseProxyRouteManifest(req.Content)
		if err != nil {
			return err
		}
		return check(manifest.Project, manifest.Environment)
	case *ReconcileProxyRequest:
		return check(req.Project, req.Environment)
	case *ProxyCertificatePushRequest:
		return check(req.Project, req.Environment)
	case *AllocationAuthorizationRequest:
		return check(req.Project, req.Environment)
	case *MetadataRequest:
		if fence.Operation != "setup" && fence.Operation != "upgrade" && fence.Operation != "platform-bootstrap" {
			return fmt.Errorf("node metadata is a cluster resource and is not authorized by %s", fence.Operation)
		}
		return nil
	default:
		return fmt.Errorf("mutation payload type %T has no endpoint-specific fence scope validator", value)
	}
}
