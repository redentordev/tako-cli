package cmd

import (
	"context"
	"fmt"

	"github.com/redentordev/tako-cli/pkg/takoapi/events"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

func init() {
	takodclient.SetReclaimObserver(reportHygieneReclaim)
}

// reportHygieneReclaim renders a reclaim the node agent client performed
// after a low-disk denial: one human line plus a hygiene.reclaimed event.
func reportHygieneReclaim(_ context.Context, reclaim takodclient.ReclaimEvent) {
	cliEngine().EventStream().Emit(hygieneReclaimEvent(reclaim))
}

func hygieneReclaimEvent(reclaim takodclient.ReclaimEvent) events.Event {
	server := reclaim.Server
	if server == "" {
		server = "server"
	}
	message := fmt.Sprintf("Disk low on %s: reclaimed %s, retrying…\n", server, takodclient.FormatBytes(reclaim.BytesFreed))
	level := events.LevelInfo
	if !reclaim.Retrying {
		message = fmt.Sprintf("Disk low on %s: reclaimed %s; the request cannot be replayed, re-run the command\n", server, takodclient.FormatBytes(reclaim.BytesFreed))
		level = events.LevelWarn
	}
	return events.Event{
		Type:    events.TypeHygieneReclaimed,
		Phase:   events.PhasePreflight,
		Level:   level,
		Node:    reclaim.Server,
		Message: message,
		Data: map[string]any{
			"server":     reclaim.Server,
			"endpoint":   reclaim.Endpoint,
			"bytesFreed": reclaim.BytesFreed,
			"retrying":   reclaim.Retrying,
		},
	}
}
