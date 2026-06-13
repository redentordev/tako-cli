package envexpand

import "testing"

func TestBracedExpandsOnlyExplicitReferences(t *testing.T) {
	got, missing := Braced("hash=$2a$10 value=${TOKEN}", func(key string) (string, bool) {
		if key == "TOKEN" {
			return "secret", true
		}
		return "", false
	})

	if len(missing) != 0 {
		t.Fatalf("missing = %#v", missing)
	}
	if got != "hash=$2a$10 value=secret" {
		t.Fatalf("expanded = %q", got)
	}
}

func TestBracedReportsSortedUniqueMissingRefs(t *testing.T) {
	_, missing := Braced("${B} ${A} ${B}", func(string) (string, bool) {
		return "", false
	})

	want := []string{"A", "B"}
	if len(missing) != len(want) {
		t.Fatalf("missing = %#v, want %#v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Fatalf("missing = %#v, want %#v", missing, want)
		}
	}
}
