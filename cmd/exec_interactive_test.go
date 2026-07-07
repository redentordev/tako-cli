package cmd

import (
	"strings"
	"testing"

	"github.com/redentordev/tako-cli/pkg/engine"
)

// TestExecInteractiveRejectedInMachineModes pins the exec -i/-t machine-mode
// contract: interactive exec exits 2 with an InvalidRequest error because raw
// terminal bytes are not events.
func TestExecInteractiveRejectedInMachineModes(t *testing.T) {
	restoreOutput, restoreEvents := outputFormatFlag, eventsFormatFlag
	restoreTTY, restoreInteractive := execTTY, execInteractive
	t.Cleanup(func() {
		outputFormatFlag, eventsFormatFlag = restoreOutput, restoreEvents
		execTTY, execInteractive = restoreTTY, restoreInteractive
	})

	for _, mode := range []struct {
		name   string
		output string
		events string
	}{
		{"output json", outputFormatJSON, ""},
		{"events ndjson", outputFormatText, eventsFormatNDJSON},
	} {
		outputFormatFlag, eventsFormatFlag = mode.output, mode.events
		execTTY, execInteractive = true, false

		err := runExecInteractive(execCmd, engine.ExecRequest{})
		if engine.Classify(err) != engine.ClassInvalid {
			t.Fatalf("%s: classified as %d, want ClassInvalid (exit 2)", mode.name, engine.Classify(err))
		}
		if !strings.Contains(err.Error(), "ptystream") {
			t.Fatalf("%s: error should point control planes at the frame protocol: %v", mode.name, err)
		}
	}
}
