package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/takod"
)

func TestImagePruneSummaryIncludesSkippedImages(t *testing.T) {
	got := imagePruneSummary("node-a", takod.ImagePruneResponse{
		Removed: []string{"demo/web:old"},
		Skipped: []string{"demo/web:live"},
	})
	if got != "node-a: pruned 1 image(s), skipped in-use: demo/web:live" {
		t.Fatalf("summary = %q", got)
	}
}

func TestRunImagePruneRequiresForce(t *testing.T) {
	oldForce := imageForce
	imageForce = false
	t.Cleanup(func() { imageForce = oldForce })

	err := runImagePrune(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "--force is required") {
		t.Fatalf("expected force error, got %v", err)
	}
}
