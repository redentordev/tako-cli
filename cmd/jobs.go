package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/redentordev/tako-cli/pkg/config"
	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/spf13/cobra"
)

var jobsServer string

var jobsCmd = &cobra.Command{
	Use:          "jobs",
	Short:        "List and run scheduled jobs",
	SilenceUsage: true,
	Long: `Manage services declared with kind: job.

Jobs run on a cron schedule inside one-off containers on a single owning
node. Deploys register the schedule with the node agent; this command shows
what is scheduled, its run history, and can trigger a run immediately.`,
	Example: `  # List scheduled jobs with their next and last runs
  tako jobs

  # Show run history for one job
  tako jobs runs report

  # Run a job right now and stream its output
  tako jobs trigger report`,
	Args: cobra.NoArgs,
	RunE: runJobsList,
}

var jobsRunsCmd = &cobra.Command{
	Use:          "runs [JOB]",
	Short:        "Show recorded job runs",
	SilenceUsage: true,
	Long: `Show the run history recorded on the environment's nodes, newest
first. Each record carries the trigger (schedule or manual), exit code,
status, and a bounded tail of the run's output.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runJobsRuns,
}

var jobsTriggerCmd = &cobra.Command{
	Use:          "trigger JOB",
	Short:        "Run a scheduled job immediately",
	SilenceUsage: true,
	Long: `Run a job now on the node holding its schedule, streaming output
until the run finishes. The run is recorded in the job's history with
trigger "manual". A run already in progress is not interrupted; the trigger
fails instead.

In the default text mode the tako process mirrors the job's exit code; in
machine modes the exit code is structured into the JobTriggerResult document
instead.`,
	Args: cobra.ExactArgs(1),
	RunE: runJobsTrigger,
}

func init() {
	rootCmd.AddCommand(jobsCmd)
	jobsCmd.AddCommand(jobsRunsCmd)
	jobsCmd.AddCommand(jobsTriggerCmd)
	jobsCmd.PersistentFlags().StringVarP(&jobsServer, "server", "s", "", "Limit to a specific node")
}

func loadJobsConfig() (*config.Config, error) {
	cfg, err := config.LoadConfig(cfgFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load config: %w", err)
	}
	if err := requireTakodRuntime(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func runJobsList(cmd *cobra.Command, args []string) error {
	cfg, err := loadJobsConfig()
	if err != nil {
		return err
	}
	result, err := cliEngine().Jobs(cmd.Context(), engine.JobsRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Server:      jobsServer,
	})
	if result != nil {
		if emitErr := renderJobsResult(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func renderJobsResult(result *engine.JobsResult) error {
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	if len(result.Jobs) == 0 {
		fmt.Println("\nNo jobs scheduled")
		return nil
	}
	fmt.Println()
	fmt.Printf("%-15s %-16s %-10s %-20s %-20s %-10s\n", "JOB", "SCHEDULE", "NODE", "NEXT RUN", "LAST RUN", "STATUS")
	fmt.Println(strings.Repeat("─", 100))
	for _, job := range result.Jobs {
		nextRun := "-"
		if job.NextRun != nil {
			nextRun = job.NextRun.Local().Format("2006-01-02 15:04:05")
		}
		lastRun := "-"
		lastStatus := "-"
		if job.LastRun != nil {
			lastRun = job.LastRun.StartedAt.Local().Format("2006-01-02 15:04:05")
			lastStatus = job.LastRun.Status
		}
		fmt.Printf("%-15s %-16s %-10s %-20s %-20s %-10s\n", job.Name, job.Schedule, job.Server, nextRun, lastRun, lastStatus)
	}
	fmt.Println()
	return nil
}

func runJobsRuns(cmd *cobra.Command, args []string) error {
	cfg, err := loadJobsConfig()
	if err != nil {
		return err
	}
	job := ""
	if len(args) > 0 {
		job = args[0]
	}
	result, err := cliEngine().JobRuns(cmd.Context(), engine.JobRunsRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Job:         job,
		Server:      jobsServer,
	})
	if result != nil {
		if emitErr := renderJobRunsResult(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	return err
}

func renderJobRunsResult(result *engine.JobRunsResult) error {
	if machineOutputEnabled() {
		return emitResultDocument(result)
	}
	if len(result.Runs) == 0 {
		fmt.Println("\nNo recorded runs")
		return nil
	}
	fmt.Println()
	fmt.Printf("%-15s %-20s %-10s %-10s %-6s %-10s\n", "JOB", "STARTED", "TRIGGER", "STATUS", "EXIT", "DURATION")
	fmt.Println(strings.Repeat("─", 80))
	for _, run := range result.Runs {
		fmt.Printf("%-15s %-20s %-10s %-10s %-6d %-10s\n",
			run.Job,
			run.StartedAt.Local().Format("2006-01-02 15:04:05"),
			run.Trigger,
			run.Status,
			run.ExitCode,
			(time.Duration(run.DurationMs) * time.Millisecond).Round(time.Millisecond).String(),
		)
	}
	fmt.Println()
	return nil
}

func runJobsTrigger(cmd *cobra.Command, args []string) error {
	cfg, err := loadJobsConfig()
	if err != nil {
		return err
	}
	result, err := cliEngine().TriggerJob(cmd.Context(), engine.JobTriggerRequest{
		Config:      cfg,
		Environment: getEnvironmentName(cfg),
		Job:         args[0],
		Server:      jobsServer,
	})
	if result != nil {
		if emitErr := emitResultDocument(result); emitErr != nil && err == nil {
			err = emitErr
		}
	}
	if err != nil {
		return err
	}
	// Machine modes structure the run's exit code in the JobTriggerResult;
	// text mode mirrors it so scripts can use $?.
	if result != nil && result.ExitCode != 0 && !machineOutputEnabled() {
		return &engine.RemoteExitError{Code: result.ExitCode}
	}
	return nil
}
