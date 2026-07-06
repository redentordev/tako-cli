package engine

import (
	"testing"
)

func TestParseScaleTargetsParsesServiceReplicaPairs(t *testing.T) {
	targets, err := ParseScaleTargets([]string{"web=5", "api = 3", "worker=0"})
	if err != nil {
		t.Fatalf("ParseScaleTargets returned error: %v", err)
	}
	if len(targets) != 3 || targets["web"] != 5 || targets["api"] != 3 || targets["worker"] != 0 {
		t.Fatalf("targets = %#v, want web=5 api=3 worker=0", targets)
	}
}

func TestParseScaleTargetsRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{name: "missing equals", args: []string{"web"}},
		{name: "non-numeric replicas", args: []string{"web=abc"}},
		{name: "negative replicas", args: []string{"web=-1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseScaleTargets(tc.args); err == nil {
				t.Fatalf("ParseScaleTargets(%v) succeeded, want error", tc.args)
			} else if Classify(err) != ClassInvalid {
				t.Fatalf("Classify(%v) = %d, want ClassInvalid", err, Classify(err))
			}
		})
	}
}
