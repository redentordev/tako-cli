package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/reconcile"
)

func TestSelectPromotionRevisionUsesUniqueWarmedRevision(t *testing.T) {
	got, err := selectPromotionRevision(&reconcile.ActualService{
		CurrentRevision:  "rev-blue",
		PreviousRevision: "rev-green",
		WarmingRevisions: []string{"rev-green"},
	}, "")
	if err != nil {
		t.Fatalf("selectPromotionRevision returned error: %v", err)
	}
	if got != "rev-green" {
		t.Fatalf("revision = %q, want rev-green", got)
	}
}

func TestSelectPromotionRevisionRequiresRevisionWhenMultipleWarmed(t *testing.T) {
	_, err := selectPromotionRevision(&reconcile.ActualService{
		CurrentRevision:  "rev-blue",
		WarmingRevisions: []string{"rev-green-a", "rev-green-b"},
	}, "")
	if err == nil {
		t.Fatal("expected multiple warmed revisions to require --revision")
	}
	if !strings.Contains(err.Error(), "multiple warmed revisions") {
		t.Fatalf("error = %v, want multiple warmed revisions", err)
	}
}

func TestSelectPromotionRevisionMatchesUniquePrefix(t *testing.T) {
	got, err := selectPromotionRevision(&reconcile.ActualService{
		CurrentRevision:  "rev-blue",
		WarmingRevisions: []string{"abcdef123456", "123456abcdef"},
	}, "abcdef")
	if err != nil {
		t.Fatalf("selectPromotionRevision returned error: %v", err)
	}
	if got != "abcdef123456" {
		t.Fatalf("revision = %q, want abcdef123456", got)
	}
}

func TestSelectPromotionRevisionRejectsCurrentRevision(t *testing.T) {
	_, err := selectPromotionRevision(&reconcile.ActualService{
		CurrentRevision:  "rev-blue",
		WarmingRevisions: []string{"rev-green"},
	}, "rev-blue")
	if err == nil {
		t.Fatal("expected current revision to be rejected")
	}
	if !strings.Contains(err.Error(), "is not warmed") {
		t.Fatalf("error = %v, want not warmed", err)
	}
}
