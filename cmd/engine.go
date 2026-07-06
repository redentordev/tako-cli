package cmd

import (
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/ssh"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

var cliEngineInstance *engine.Engine

// cliEngine returns the process-wide engine wired to human terminal output.
// Machine output modes swap the sink at command setup time.
func cliEngine() *engine.Engine {
	if cliEngineInstance == nil {
		cliEngineInstance = engine.New(engine.Options{
			CLIVersion: Version,
			CLICommit:  GitCommit,
			Sink:       humanEventSink{},
			StateAutoSync: func(pool *ssh.Pool, cfg *config.Config, envName string) error {
				return SyncStateOnDeployWithPool(pool, cfg, envName)
			},
		})
	}
	return cliEngineInstance
}

// humanEventSink renders engine events as the CLI's terminal output. Event
// messages carry the exact bytes the command historically printed, so this
// sink writes them verbatim; debug-level events render only with --verbose.
type humanEventSink struct{}

func (humanEventSink) Emit(event events.Event) {
	if event.Level == events.LevelDebug && !verbose {
		return
	}
	if event.Message == "" {
		return
	}
	if stream, ok := event.Data["stream"].(string); ok && stream == "stderr" {
		fmt.Fprint(os.Stderr, event.Message)
		return
	}
	fmt.Print(event.Message)
}
