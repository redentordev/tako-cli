package cmd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/platform"
)

func testWorkerInspection() *platform.EnrollmentInspection {
	updatedAt := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	return &platform.EnrollmentInspection{
		APIVersion: platform.APIVersion, Kind: platform.EnrollmentInspectionKind, Status: platform.EnrollmentEnrolled,
		ClusterID: "cluster-a", NodeID: "node-id-2", NodeName: "node-2", IdentityVerification: platform.IdentityVerificationVerified,
		PublishedRoles: []string{"worker"}, PublishedLifecycle: "schedulable", PublishedSchedulable: true,
		InventoryGeneration: 7, InventoryUpdatedAt: &updatedAt,
		PublishedControllerID: "node-id-1", PublishedControllerName: "node-1", NextAction: platform.EnrollmentActionUseExisting,
		Warnings: []string{"snapshot does not prove reachability"},
	}
}

func TestRenderPlatformInspectionExplainsRemotePublishedAuthority(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderPlatformInspection(testWorkerInspection()); err != nil {
			t.Fatalf("render returned error: %v", err)
		}
	})
	for _, required := range []string{"Existing Tako platform enrollment detected", "Node: node-2 (node-id-2)", "Last published controller: node-1 (node-id-1)", "Published authority: remote", "does not promote this node", "does not prove reachability", "another server or VM"} {
		if !strings.Contains(out, required) {
			t.Fatalf("inspection output lacks %q:\n%s", required, out)
		}
	}
}

func TestRenderPlatformInspectionExplainsUnenrolledHost(t *testing.T) {
	out := captureStdout(t, func() {
		if err := renderPlatformInspection(&platform.EnrollmentInspection{Status: platform.EnrollmentNotEnrolled, NextAction: platform.EnrollmentActionInitialize, NextCommand: "sudo tako platform init"}); err != nil {
			t.Fatalf("render returned error: %v", err)
		}
	})
	if !strings.Contains(out, "No Tako platform enrollment detected") || !strings.Contains(out, "sudo tako platform init") || !strings.Contains(out, "No protected identity") {
		t.Fatalf("unexpected unenrolled output:\n%s", out)
	}
}

func TestRenderPlatformInspectionRecommendsInterruptedInitResume(t *testing.T) {
	inspection := &platform.EnrollmentInspection{
		Status: platform.EnrollmentIncomplete, ClusterID: "cluster-a", NodeID: "node-id-1", NodeName: "node-1",
		NextAction: platform.EnrollmentActionResumeInit, NextCommand: "sudo tako platform init --node node-1 --cluster-id cluster-a --node-id node-id-1", Detail: "public binding is missing",
	}
	out := captureStdout(t, func() {
		if err := renderPlatformInspection(inspection); err != nil {
			t.Fatalf("render returned error: %v", err)
		}
	})
	if !strings.Contains(out, "Safe resume command") || strings.Contains(out, "Do not initialize over this state") {
		t.Fatalf("wrong interrupted-init guidance:\n%s", out)
	}
}

func TestDeliverPlatformInspectionEmitsJSONAndAttentionExit(t *testing.T) {
	restoreOutput, restoreEvents := outputFormatFlag, eventsFormatFlag
	outputFormatFlag, eventsFormatFlag = outputFormatJSON, ""
	t.Cleanup(func() { outputFormatFlag, eventsFormatFlag = restoreOutput, restoreEvents })
	inspection := &platform.EnrollmentInspection{
		APIVersion: platform.APIVersion, Kind: platform.EnrollmentInspectionKind, Status: platform.EnrollmentIncomplete,
		NextAction: platform.EnrollmentActionResumeInit, NextCommand: "sudo tako platform init", Detail: "interrupted bootstrap",
	}
	var runErr error
	stdout := captureStdout(t, func() { runErr = deliverPlatformInspection(inspection) })
	if engine.Classify(runErr) != engine.ClassAttention {
		t.Fatalf("incomplete inspection error = %v, class %d", runErr, engine.Classify(runErr))
	}
	var decoded platform.EnrollmentInspection
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatalf("decode JSON %q: %v", stdout, err)
	}
	if decoded.Status != platform.EnrollmentIncomplete || decoded.NextAction != platform.EnrollmentActionResumeInit {
		t.Fatalf("machine inspection = %#v", decoded)
	}
}

func TestDeliverPlatformInspectionJSONSuccess(t *testing.T) {
	restoreOutput, restoreEvents := outputFormatFlag, eventsFormatFlag
	outputFormatFlag, eventsFormatFlag = outputFormatJSON, ""
	t.Cleanup(func() { outputFormatFlag, eventsFormatFlag = restoreOutput, restoreEvents })
	stdout := captureStdout(t, func() {
		if err := deliverPlatformInspection(testWorkerInspection()); err != nil {
			t.Fatalf("deliver returned error: %v", err)
		}
	})
	if strings.Contains(stdout, "Existing Tako") || !strings.Contains(stdout, `"status": "enrolled"`) || !strings.Contains(stdout, `"inventoryUpdatedAt"`) {
		t.Fatalf("unexpected JSON output:\n%s", stdout)
	}
}
