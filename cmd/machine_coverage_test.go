package cmd

import (
	"errors"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

// The category sets below mirror the command coverage table in
// docs/MACHINE-INTERFACE.md. TestMachineCommandCoverage walks the registered
// command tree and fails when a runnable command is missing from the table or
// listed twice, so new commands must declare their machine-output category.

var machineFullContractCommands = map[string]bool{
	"tako access":                   true,
	"tako backup":                   true,
	"tako cleanup":                  true,
	"tako certs ls":                 true,
	"tako certs push":               true,
	"tako certs rm":                 true,
	"tako clone-setup":              true,
	"tako config export":            true,
	"tako config pull":              true,
	"tako deploy":                   true,
	"tako destroy":                  true,
	"tako discovery exports":        true,
	"tako doctor":                   true,
	"tako domains hosts":            true,
	"tako domains status":           true,
	"tako drift":                    true,
	"tako exec":                     true,
	"tako history":                  true,
	"tako jobs":                     true,
	"tako jobs runs":                true,
	"tako jobs trigger":             true,
	"tako live":                     true,
	"tako logs":                     true,
	"tako maintenance":              true,
	"tako metrics":                  true,
	"tako promote":                  true,
	"tako project attach":           true,
	"tako proxy hash-password":      true,
	"tako ps":                       true,
	"tako remove":                   true,
	"tako rollback":                 true,
	"tako run":                      true,
	"tako scale":                    true,
	"tako secrets list":             true,
	"tako secrets validate":         true,
	"tako setup":                    true,
	"tako start":                    true,
	"tako state forget-node":        true,
	"tako state lease":              true,
	"tako state lease release":      true,
	"tako state pull":               true,
	"tako state repair":             true,
	"tako state status":             true,
	"tako stats":                    true,
	"tako stop":                     true,
	"tako placement plan drain":     true,
	"tako placement plan cordon":    true,
	"tako placement plan rebalance": true,
	"tako placement apply":          true,
	"tako placement verify":         true,
	"tako platform inspect":         true,
	"tako upgrade servers":          true,
	"tako validate":                 true,
}

// machineNativeCommands emit a machine-native format on stdout without the
// result-document contract.
var machineNativeCommands = map[string]bool{
	"tako prometheus": true,
}

// humanOnlyCommands must carry the human-only annotation so machine modes
// are rejected with exit code 2 instead of corrupting stdout.
var humanOnlyCommands = map[string]bool{
	"tako config explain": true,
	"tako env pull":       true,
	"tako env push":       true,
	"tako init":           true,
	"tako monitor":        true,
	"tako platform init":  true,
	"tako platform controller promotion verify": true,
	"tako platform backup create":               true,
	"tako platform backup verify":               true,
	"tako platform backup restore":              true,
	"tako platform join-token create":           true,
	"tako platform node cordon":                 true,
	"tako platform node drain":                  true,
	"tako platform node enroll":                 true,
	"tako platform node list":                   true,
	"tako platform node ready":                  true,
	"tako platform node remove":                 true,
	"tako platform node schedulable":            true,
	"tako secrets delete":                       true,
	"tako secrets fetch":                        true,
	"tako secrets import":                       true,
	"tako secrets init":                         true,
	"tako secrets set":                          true,
	"tako upgrade":                              true,
}

// infrastructureCommands are not part of the CLI's operator surface: the
// node daemon runner and hidden e2e helpers. Cobra's built-in help and
// completion commands are registered lazily at execution time and are
// likewise out of scope.
var infrastructureCommands = map[string]bool{
	"tako internal e2e-server-ssh":                 true,
	"tako platform node upgrade-publication-guard": true,
	"tako platform worker run":                     true,
	"tako platform worker prepare-enrollment":      true,
	"tako platform worker reconcile-mesh":          true,
	"tako platform worker verify-enrollment":       true,
	"tako takod run":                               true,
}

func runnableCommandPaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	var walk func(command *cobra.Command)
	walk = func(command *cobra.Command) {
		if command.Run != nil || command.RunE != nil {
			paths = append(paths, command.CommandPath())
		}
		for _, child := range command.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)
	return paths
}

// TestMachineCommandCoverage keeps the docs/MACHINE-INTERFACE.md coverage
// table honest: every runnable command belongs to exactly one category and
// every category entry names a real command.
func TestMachineCommandCoverage(t *testing.T) {
	categories := map[string]map[string]bool{
		"full contract":  machineFullContractCommands,
		"machine native": machineNativeCommands,
		"human only":     humanOnlyCommands,
		"infrastructure": infrastructureCommands,
	}

	seen := map[string]bool{}
	for _, path := range runnableCommandPaths(t) {
		seen[path] = true
		var memberships []string
		for name, set := range categories {
			if set[path] {
				memberships = append(memberships, name)
			}
		}
		switch len(memberships) {
		case 0:
			t.Errorf("%s is not categorized in the machine-output coverage table; add it to docs/MACHINE-INTERFACE.md and the matching set in this test", path)
		case 1:
		default:
			t.Errorf("%s appears in multiple coverage categories: %v", path, memberships)
		}
	}

	for name, set := range categories {
		for path := range set {
			if !seen[path] {
				t.Errorf("coverage category %q lists %s, which is not a runnable command", name, path)
			}
		}
	}
}

// TestHumanOnlyAnnotationMatchesCoverage asserts the human-only annotation is
// present on exactly the commands the coverage table declares human-only.
func TestHumanOnlyAnnotationMatchesCoverage(t *testing.T) {
	var walk func(command *cobra.Command)
	walk = func(command *cobra.Command) {
		annotated := command.Annotations[humanOnlyAnnotation] == "true"
		if annotated != humanOnlyCommands[command.CommandPath()] {
			if annotated {
				t.Errorf("%s carries the human-only annotation but is not in the human-only coverage set", command.CommandPath())
			} else {
				t.Errorf("%s is declared human-only but does not carry the annotation", command.CommandPath())
			}
		}
		for _, child := range command.Commands() {
			walk(child)
		}
	}
	walk(rootCmd)
}

// TestHumanOnlyCommandsRejectMachineOutput pins the fail-fast behavior:
// machine modes on a human-only command yield a typed invalid request (exit
// code 2), while covered commands are unaffected.
func TestHumanOnlyCommandsRejectMachineOutput(t *testing.T) {
	restoreOutput := outputFormatFlag
	restoreEvents := eventsFormatFlag
	defer func() {
		outputFormatFlag = restoreOutput
		eventsFormatFlag = restoreEvents
	}()

	cases := []struct {
		name   string
		output string
		events string
	}{
		{name: "output json", output: outputFormatJSON},
		{name: "events ndjson", output: outputFormatText, events: eventsFormatNDJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			outputFormatFlag = tc.output
			eventsFormatFlag = tc.events

			err := rejectMachineOutputForHumanOnly(initCmd)
			if err == nil {
				t.Fatal("expected human-only rejection for tako init, got nil")
			}
			var invalid *engine.InvalidRequestError
			if !errors.As(err, &invalid) {
				t.Fatalf("expected InvalidRequestError, got %T: %v", err, err)
			}
			if code := exitCodeForError(err); code != 2 {
				t.Fatalf("expected exit code 2, got %d", code)
			}

			if err := rejectMachineOutputForHumanOnly(upgradeServersCmd); err != nil {
				t.Fatalf("tako upgrade servers must keep its machine contract, got rejection: %v", err)
			}
		})
	}

	outputFormatFlag = outputFormatText
	eventsFormatFlag = ""
	if err := rejectMachineOutputForHumanOnly(initCmd); err != nil {
		t.Fatalf("text mode must not reject human-only commands, got: %v", err)
	}
}
