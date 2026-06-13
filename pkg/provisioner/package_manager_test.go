package provisioner

import (
	"strings"
	"testing"
)

func TestQuotePackageArgsQuotesEachPackage(t *testing.T) {
	args, err := quotePackageArgs("curl", "libfoo-dev", "bad;touch /tmp/pwned")
	if err != nil {
		t.Fatalf("quotePackageArgs returned error: %v", err)
	}
	for _, want := range []string{"'curl'", "'libfoo-dev'", "'bad;touch /tmp/pwned'"} {
		if !strings.Contains(args, want) {
			t.Fatalf("args = %q, want %q", args, want)
		}
	}
}

func TestQuotePackageArgRejectsEmptyPackage(t *testing.T) {
	if _, err := quotePackageArg("  "); err == nil {
		t.Fatal("quotePackageArg should reject empty package names")
	}
}
