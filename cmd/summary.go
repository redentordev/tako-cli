package cmd

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/redentordev/tako-cli/pkg/engine"
	"github.com/redentordev/tako-cli/pkg/takoapi/events"
)

const (
	// stepSummaryMaxBytes caps the Markdown one operation appends to the
	// GitHub step summary file.
	stepSummaryMaxBytes = 64 * 1024
	// stepSummaryMaxRows caps the service rows in the step summary table.
	stepSummaryMaxRows = 200
	// summaryRuleWidth is the width of the text block's horizontal rules.
	summaryRuleWidth = 62
)

// emitOperationSummary delivers the end-of-operation summary: one
// operation.summary event (rendered as the human block by the text sink,
// and as an NDJSON line before the result event) plus the GitHub Actions
// step summary when enabled. It must run after hook outcomes are attached
// and immediately before emitResultDocument.
func emitOperationSummary(summary *engine.OperationSummary, hookOutcomes []engine.HookOutcome) {
	if summary == nil {
		return
	}
	addHookNotes(summary, hookOutcomes)
	withConfigFlag(summary, cfgFile)
	eng := cliEngine()
	eng.EventStream().Emit(events.Event{
		Type:    events.TypeOperationSummary,
		Phase:   events.PhaseSummary,
		Level:   events.LevelInfo,
		Message: renderSummaryText(summary, logsColorEnabled(os.Stdout)),
		Data:    map[string]any{"summary": summary, "operation": summary.Operation},
	})
	writeGitHubStepSummary(summary, eng.EventStream())
}

// addHookNotes mentions failed or timed-out lifecycle hooks in the notes.
func addHookNotes(summary *engine.OperationSummary, outcomes []engine.HookOutcome) {
	for _, outcome := range outcomes {
		var note string
		switch outcome.Status {
		case engine.HookFailed:
			if outcome.ExitCode != nil {
				note = fmt.Sprintf("%s hook failed (exit %d)", outcome.Name, *outcome.ExitCode)
			} else {
				note = fmt.Sprintf("%s hook failed", outcome.Name)
			}
		case engine.HookTimedOut:
			note = fmt.Sprintf("%s hook timed out", outcome.Name)
		default:
			continue
		}
		if !slices.Contains(summary.Notes, note) {
			summary.Notes = append(summary.Notes, note)
		}
	}
}

// withConfigFlag makes the undo and next-step commands select the config
// file the operator used, so they act on the same project when pasted.
func withConfigFlag(summary *engine.OperationSummary, configPath string) {
	if summary == nil || configPath == "" {
		return
	}
	if undo := summary.Undo; undo != nil && len(undo.Argv) > 0 && undo.Argv[0] == "tako" {
		if len(undo.Argv) < 2 || undo.Argv[1] != "--config" {
			argv := append([]string{"tako", "--config", configPath}, undo.Argv[1:]...)
			undo.Argv = argv
			undo.Command = engine.ShellJoin(argv)
		}
	}
	prefix := "tako --config "
	for index := range summary.NextSteps {
		command := summary.NextSteps[index].Command
		if strings.HasPrefix(command, "tako ") && !strings.HasPrefix(command, prefix) {
			summary.NextSteps[index].Command = prefix + engine.ShellQuote(configPath) + strings.TrimPrefix(command, "tako")
		}
	}
}

// summaryFailed reports whether the summary describes a failed operation.
func summaryFailed(summary *engine.OperationSummary) bool {
	return summary.Outcome == engine.SummaryOutcomeFailed || summary.Outcome == engine.SummaryOutcomePartial
}

// summaryHeadline is the header sentence without the icon.
func summaryHeadline(summary *engine.OperationSummary) string {
	target := fmt.Sprintf("%s → %s", summary.Project, summary.Environment.Name)
	if summary.Outcome == engine.SummaryOutcomeUpToDate {
		return fmt.Sprintf("%s is up to date (%s)", target, summary.Operation)
	}
	if summaryFailed(summary) {
		verb := map[string]string{
			engine.SummaryOperationDeploy:   "Deploy",
			engine.SummaryOperationRollback: "Rollback",
			engine.SummaryOperationPromote:  "Promote",
			engine.SummaryOperationScale:    "Scale",
		}[summary.Operation]
		if verb == "" {
			verb = summary.Operation
		}
		headline := fmt.Sprintf("%s failed", verb)
		if summary.Outcome == engine.SummaryOutcomePartial {
			headline += " (partial)"
		}
		return fmt.Sprintf("%s: %s", headline, target)
	}
	verb := map[string]string{
		engine.SummaryOperationDeploy:   "Deployed",
		engine.SummaryOperationRollback: "Rolled back",
		engine.SummaryOperationPromote:  "Promoted",
		engine.SummaryOperationScale:    "Scaled",
	}[summary.Operation]
	if verb == "" {
		verb = summary.Operation
	}
	if summary.Outcome == engine.SummaryOutcomeWarmed {
		verb = "Warmed"
	}
	return fmt.Sprintf("%s %s", verb, target)
}

// summaryMeta lists the duration, display ID and revision.
func summaryMeta(summary *engine.OperationSummary) []string {
	var meta []string
	meta = append(meta, "in "+formatSummaryDuration(summary.DurationSeconds))
	if summary.DisplayID != "" {
		meta = append(meta, "deployment "+summary.DisplayID)
	}
	if summary.Revision != "" {
		meta = append(meta, summary.Revision)
	} else if summary.Commit != "" {
		meta = append(meta, summary.Commit)
	}
	return meta
}

func formatSummaryDuration(seconds float64) string {
	if seconds <= 0 {
		return "0s"
	}
	duration := time.Duration(seconds * float64(time.Second))
	if duration < time.Second {
		return duration.Round(time.Millisecond).String()
	}
	return duration.Round(time.Second).String()
}

// summaryImageCell is the IMAGE column: the image once when unchanged, else
// `repo:tagA → tagB` when both refs share the repository. Run rows show
// their reason (exit code and duration).
func summaryImageCell(row engine.SummaryService) string {
	if row.Change == engine.SummaryChangeRan {
		if row.Reason != "" {
			return row.Reason
		}
		return "-"
	}
	from, to := row.FromImage, row.ToImage
	switch {
	case from == "" && to == "":
		return "-"
	case from == "" || from == to:
		return to
	case to == "":
		return from
	}
	fromRepo, _ := splitImageTag(from)
	toRepo, toTag := splitImageTag(to)
	if fromRepo == toRepo && toTag != "" {
		return from + " → " + toTag
	}
	return from + " → " + to
}

// splitImageTag splits an image reference into repository and tag (or
// digest). A colon in the registry host port is not a tag separator.
func splitImageTag(ref string) (string, string) {
	if index := strings.Index(ref, "@"); index >= 0 {
		return ref[:index], ref[index+1:]
	}
	slash := strings.LastIndex(ref, "/")
	if index := strings.LastIndex(ref, ":"); index > slash {
		return ref[:index], ref[index+1:]
	}
	return ref, ""
}

func summaryReplicasCell(row engine.SummaryService) string {
	if row.Replicas == nil {
		return "-"
	}
	return fmt.Sprintf("%d/%d", row.Replicas.Running, row.Replicas.Desired)
}

func summaryDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

// summaryDetailLines are per-row reasons and errors that do not fit the table.
func summaryDetailLines(summary *engine.OperationSummary) []string {
	var lines []string
	for _, row := range summary.Services {
		if row.Reason != "" && row.Change != engine.SummaryChangeRan {
			lines = append(lines, fmt.Sprintf("%s: %s", row.Name, row.Reason))
		}
		if row.Error != "" && row.Error != summary.Error {
			lines = append(lines, fmt.Sprintf("%s: %s", row.Name, row.Error))
		}
	}
	return lines
}

const (
	ansiReset  = "\033[0m"
	ansiGreen  = "\033[32m"
	ansiRed    = "\033[31m"
	ansiYellow = "\033[33m"
	ansiBold   = "\033[1m"
)

// renderSummaryText renders the human summary block. color adds ANSI
// styling and is only set for a terminal without NO_COLOR.
func renderSummaryText(summary *engine.OperationSummary, color bool) string {
	if summary == nil {
		return ""
	}
	paint := func(code string, text string) string {
		if !color {
			return text
		}
		return code + text + ansiReset
	}
	icon, iconColor := "✓", ansiGreen
	switch {
	case summary.Outcome == engine.SummaryOutcomePartial:
		icon, iconColor = "✗", ansiRed
	case summary.Outcome == engine.SummaryOutcomeFailed:
		icon, iconColor = "✗", ansiRed
	case summary.Outcome == engine.SummaryOutcomeWarmed:
		icon, iconColor = "!", ansiYellow
	}
	rule := strings.Repeat("─", summaryRuleWidth)

	var out bytes.Buffer
	out.WriteString("\n" + rule + "\n")
	header := summaryHeadline(summary)
	if summary.Outcome != engine.SummaryOutcomeUpToDate || summary.DisplayID != "" {
		header += " " + strings.Join(summaryMeta(summary), " · ")
	}
	fmt.Fprintf(&out, " %s %s\n", paint(iconColor, icon), paint(ansiBold, header))
	out.WriteString(rule + "\n")

	if len(summary.Services) > 0 {
		table := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(table, " SERVICE\tCHANGE\tSTRATEGY\tIMAGE\tREPLICAS\tHEALTH")
		for _, row := range summary.Services {
			fmt.Fprintf(table, " %s\t%s\t%s\t%s\t%s\t%s\n", row.Name, row.Change, summaryDash(row.Strategy), summaryImageCell(row), summaryReplicasCell(row), row.Health)
		}
		table.Flush()
	}

	if details := summaryDetailLines(summary); len(details) > 0 {
		out.WriteString("\n Details\n")
		for _, line := range details {
			fmt.Fprintf(&out, "   %s\n", line)
		}
	}

	if len(summary.URLs) > 0 {
		out.WriteString("\n URLs\n")
		table := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
		for _, url := range summary.URLs {
			status := url.Status
			if status == "" {
				status = url.Kind
			}
			if url.Detail != "" {
				status += " (" + url.Detail + ")"
			}
			fmt.Fprintf(table, "   %s\t%s\t%s\n", url.Service, url.URL, status)
		}
		table.Flush()
	}

	if summary.Error != "" {
		fmt.Fprintf(&out, "\n %s %s\n", paint(ansiRed, "Error:"), summary.Error)
	}

	if len(summary.Notes) > 0 {
		out.WriteString("\n Notes\n")
		for _, note := range summary.Notes {
			fmt.Fprintf(&out, "   %s\n", note)
		}
	}

	if len(summary.NextSteps) > 0 {
		out.WriteString("\n Next steps\n")
		for _, step := range summary.NextSteps {
			fmt.Fprintf(&out, "   %s  # %s\n", step.Command, step.Reason)
		}
	}

	if undo := summary.Undo; undo != nil {
		out.WriteString("\n Undo\n")
		if undo.Command != "" {
			fmt.Fprintf(&out, "   %s\n", undo.Command)
		} else if undo.Unavailable != "" {
			fmt.Fprintf(&out, "   unavailable: %s\n", undo.Unavailable)
		}
		for _, note := range undo.Notes {
			fmt.Fprintf(&out, "   note: %s\n", note)
		}
	}
	out.WriteString(rule + "\n")
	return out.String()
}

// stepSummaryEnabled reports whether the GitHub step summary file should be
// written, and its path.
func stepSummaryEnabled() (string, bool) {
	path := os.Getenv("GITHUB_STEP_SUMMARY")
	if strings.TrimSpace(path) == "" {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TAKO_STEP_SUMMARY"))) {
	case "off", "0", "false", "no":
		return "", false
	}
	return path, true
}

// writeGitHubStepSummary appends the Markdown rendering of the summary to
// the GITHUB_STEP_SUMMARY file. A failure is a warning event and never
// changes the exit code.
func writeGitHubStepSummary(summary *engine.OperationSummary, sink events.Sink) {
	if summary == nil {
		return
	}
	path, ok := stepSummaryEnabled()
	if !ok {
		return
	}
	content := renderSummaryMarkdown(summary)
	err := appendStepSummary(path, content)
	if err != nil && sink != nil {
		sink.Emit(events.Event{
			Type:    events.TypeWarning,
			Phase:   events.PhaseSummary,
			Level:   events.LevelWarn,
			Message: fmt.Sprintf("Warning: step summary not written: %v\n", err),
			Data:    map[string]any{"error": err.Error()},
		})
	}
}

func appendStepSummary(path string, content string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// escapeMarkdown escapes the characters that would break a table cell or be
// read as HTML or code in free text.
func escapeMarkdown(text string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"|", "\\|",
		"<", "&lt;",
		">", "&gt;",
		"`", "\\`",
		"\r", "",
		"\n", " ",
	)
	return replacer.Replace(text)
}

// markdownCode renders text as an inline code span when it is safe to,
// else as escaped text.
func markdownCode(text string) string {
	if text == "" || strings.ContainsAny(text, "`|\n\r") {
		return escapeMarkdown(text)
	}
	return "`" + text + "`"
}

// markdownFence returns a backtick fence longer than any backtick run in text.
func markdownFence(text string) string {
	longest, run := 0, 0
	for _, char := range text {
		if char == '`' {
			run++
			if run > longest {
				longest = run
			}
			continue
		}
		run = 0
	}
	if longest < 3 {
		return "```"
	}
	return strings.Repeat("`", longest+1)
}

// renderSummaryMarkdown renders the summary for the GitHub Actions job
// summary, capped at stepSummaryMaxRows rows and stepSummaryMaxBytes bytes.
func renderSummaryMarkdown(summary *engine.OperationSummary) string {
	var out strings.Builder
	icon := "✅"
	title := fmt.Sprintf("Tako %s · %s → %s", summary.Operation, summary.Project, summary.Environment.Name)
	switch summary.Outcome {
	case engine.SummaryOutcomeFailed:
		icon, title = "❌", title+" failed"
	case engine.SummaryOutcomePartial:
		icon, title = "⚠️", title+" partial"
	case engine.SummaryOutcomeUpToDate:
		title += " up to date"
	case engine.SummaryOutcomeWarmed:
		title += " warmed"
	}
	fmt.Fprintf(&out, "### %s %s\n\n", icon, escapeMarkdown(title))

	meta := []string{"Outcome: " + summary.Outcome, "Duration: " + formatSummaryDuration(summary.DurationSeconds)}
	if summary.DisplayID != "" {
		meta = append(meta, "Deployment: "+markdownCode(summary.DisplayID))
	}
	if summary.Revision != "" {
		meta = append(meta, "Revision: "+markdownCode(summary.Revision))
	}
	if summary.Commit != "" && summary.Commit != summary.Revision {
		meta = append(meta, "Commit: "+markdownCode(summary.Commit))
	}
	out.WriteString(strings.Join(meta, " · ") + "\n\n")

	if len(summary.Services) > 0 {
		out.WriteString("| Service | Change | Strategy | Image | Replicas | Health |\n")
		out.WriteString("| --- | --- | --- | --- | --- | --- |\n")
		for index, row := range summary.Services {
			if index >= stepSummaryMaxRows {
				fmt.Fprintf(&out, "\n_%d more services not shown._\n", len(summary.Services)-stepSummaryMaxRows)
				break
			}
			image := summaryImageCell(row)
			if row.Change != engine.SummaryChangeRan && image != "-" {
				image = markdownCode(image)
			} else {
				image = escapeMarkdown(image)
			}
			fmt.Fprintf(&out, "| %s | %s | %s | %s | %s | %s |\n",
				escapeMarkdown(row.Name), escapeMarkdown(row.Change), escapeMarkdown(summaryDash(row.Strategy)),
				image, summaryReplicasCell(row), escapeMarkdown(row.Health))
		}
		out.WriteString("\n")
	}

	if details := summaryDetailLines(summary); len(details) > 0 {
		out.WriteString("**Details**\n\n")
		for _, line := range details {
			fmt.Fprintf(&out, "- %s\n", escapeMarkdown(line))
		}
		out.WriteString("\n")
	}

	if len(summary.URLs) > 0 {
		out.WriteString("**URLs**\n\n")
		for _, url := range summary.URLs {
			status := url.Status
			if status == "" {
				status = url.Kind
			}
			if url.Detail != "" {
				status += ": " + url.Detail
			}
			fmt.Fprintf(&out, "- %s: %s (%s)\n", escapeMarkdown(url.Service), escapeMarkdown(url.URL), escapeMarkdown(status))
		}
		out.WriteString("\n")
	}

	if len(summary.NextSteps) > 0 {
		out.WriteString("**Next steps**\n\n")
		for _, step := range summary.NextSteps {
			fmt.Fprintf(&out, "- %s: %s\n", escapeMarkdown(step.Reason), markdownCode(step.Command))
		}
		out.WriteString("\n")
	}

	if len(summary.Notes) > 0 {
		out.WriteString("**Notes**\n\n")
		for _, note := range summary.Notes {
			fmt.Fprintf(&out, "- %s\n", escapeMarkdown(note))
		}
		out.WriteString("\n")
	}

	if undo := summary.Undo; undo != nil {
		out.WriteString("**Undo**\n\n")
		if undo.Command != "" {
			fence := markdownFence(undo.Command)
			fmt.Fprintf(&out, "%ssh\n%s\n%s\n\n", fence, undo.Command, fence)
		} else if undo.Unavailable != "" {
			fmt.Fprintf(&out, "Unavailable: %s\n\n", escapeMarkdown(undo.Unavailable))
		}
		for _, note := range undo.Notes {
			fmt.Fprintf(&out, "- %s\n", escapeMarkdown(note))
		}
		if len(undo.Notes) > 0 {
			out.WriteString("\n")
		}
	}

	if summary.Error != "" {
		fence := markdownFence(summary.Error)
		fmt.Fprintf(&out, "<details><summary>Error</summary>\n\n%s\n%s\n%s\n\n</details>\n\n", fence, summary.Error, fence)
	}
	return truncateStepSummary(out.String())
}

const stepSummaryTruncationNote = "\n\n_Tako summary truncated at 64 KiB._\n"

// truncateStepSummary cuts content to stepSummaryMaxBytes on a UTF-8
// boundary, ending with a truncation note.
func truncateStepSummary(content string) string {
	if len(content) <= stepSummaryMaxBytes {
		return content
	}
	limit := stepSummaryMaxBytes - len(stepSummaryTruncationNote)
	for limit > 0 && !utf8.RuneStart(content[limit]) {
		limit--
	}
	return content[:limit] + stepSummaryTruncationNote
}
