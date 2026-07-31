package cmd

import (
	"fmt"
	"strings"

	"github.com/redentordev/tako-cli/pkg/takod"
	"github.com/spf13/cobra"
)

var backupRecoverJournal string
var backupRecoverAction string
var backupRecoverYes bool

var backupRecoverCmd = &cobra.Command{
	Use:   "recover",
	Short: "Recover an interrupted local volume restore",
	Long: `Recover an interrupted volume restore directly on the affected node.

This command does not contact takod, so it remains available when fail-closed
restore recovery prevents the daemon from starting. Run inspect first. Accept
keeps current target contents, rollback restores the recorded pre-restore
archive for a pre-existing target, and abandon keeps a transaction-created
target while removing the blocking transaction evidence.

Schema-v1 journals are refused because they lack target identity evidence. Keep
takod stopped, copy the complete environment backup directory, independently
match the target with docker volume inspect and deployment inventory, then
follow the legacy manual procedure in the configuration documentation. Never
delete the target automatically.`,
	Example: `  sudo tako backup recover --journal /var/lib/tako/backups/myapp/production/.restore-transaction-ID.json --action inspect
  sudo tako backup recover --journal /var/lib/tako/backups/myapp/production/.restore-transaction-ID.json --action rollback --yes`,
	Args: cobra.NoArgs,
	RunE: runBackupRecover,
}

func init() {
	backupCmd.AddCommand(backupRecoverCmd)
	backupRecoverCmd.Flags().StringVar(&backupRecoverJournal, "journal", "", "Absolute restore transaction journal path")
	backupRecoverCmd.Flags().StringVar(&backupRecoverAction, "action", "inspect", "Recovery action: inspect, accept, rollback, or abandon")
	backupRecoverCmd.Flags().BoolVarP(&backupRecoverYes, "yes", "y", false, "Approve the selected recovery action")
	markHumanOnly(backupRecoverCmd)
}

func runBackupRecover(cmd *cobra.Command, _ []string) error {
	if strings.TrimSpace(backupRecoverJournal) == "" {
		return fmt.Errorf("--journal is required")
	}
	action := strings.ToLower(strings.TrimSpace(backupRecoverAction))
	if action != "inspect" && !backupRecoverYes {
		return fmt.Errorf("--yes is required for recovery action %s", action)
	}
	result, err := takod.RecoverPendingVolumeRestore(cmd.Context(), backupRecoverJournal, action)
	if err != nil {
		return err
	}
	if action == "inspect" {
		fmt.Fprintf(humanOut(), "Restore journal: %s\nTarget volume: %s\nPhase: %s\nCreated target: %t\nIdentity: verified\n", result.Journal, result.TargetVolume, result.Phase, result.CreatedTarget)
		return nil
	}
	fmt.Fprintf(humanOut(), "Recovered restore transaction with action %s; target volume %s was preserved.\n", result.Action, result.TargetVolume)
	return nil
}
