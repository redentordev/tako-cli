package formatter

import (
	"fmt"
	"strings"
)

// Color codes for terminal output
const (
	ColorReset  = "\033[0m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorPurple = "\033[35m"
	ColorCyan   = "\033[36m"
	ColorGray   = "\033[37m"
	ColorWhite  = "\033[97m"

	ColorBold      = "\033[1m"
	ColorDim       = "\033[2m"
	ColorUnderline = "\033[4m"
)

// Icons for different message types
const (
	IconSuccess  = "✓"
	IconError    = "✗"
	IconWarning  = "⚠️"
	IconInfo     = "→"
	IconPackage  = "📦"
	IconTrash    = "🗑️"
	IconTools    = "🔧"
	IconRocket   = "🚀"
	IconClock    = "⏰"
	IconCheck    = "✅"
	IconCross    = "❌"
	IconQuestion = "❓"
)

// Output provides formatted output methods
type Output struct {
	verbose bool
	noColor bool
}

// New creates a new Output formatter
func New(verbose, noColor bool) *Output {
	return &Output{
		verbose: verbose,
		noColor: noColor,
	}
}

// color applies color to text if colors are enabled
func (o *Output) color(color, text string) string {
	if o.noColor {
		return text
	}
	return color + text + ColorReset
}

// Success prints a success message
func (o *Output) Success(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s\n", o.color(ColorGreen, IconSuccess), msg)
}

// Error prints an error message
func (o *Output) Error(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s\n", o.color(ColorRed, IconError), msg)
}

// Warning prints a warning message
func (o *Output) Warning(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s\n", o.color(ColorYellow, IconWarning), msg)
}

// Info prints an info message
func (o *Output) Info(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s\n", o.color(ColorBlue, IconInfo), msg)
}

// Verbose prints a message only if verbose mode is enabled
func (o *Output) Verbose(format string, args ...interface{}) {
	if o.verbose {
		msg := fmt.Sprintf(format, args...)
		fmt.Printf("  %s\n", o.color(ColorDim, msg))
	}
}

// Section prints a section header
func (o *Output) Section(title string) {
	fmt.Printf("\n%s\n\n", o.color(ColorBold, "=== "+title+" ==="))
}

// Subsection prints a subsection header
func (o *Output) Subsection(title string) {
	fmt.Printf("\n%s\n", o.color(ColorCyan, title+":"))
}

// Step prints a step message
func (o *Output) Step(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("%s %s\n", o.color(ColorCyan, IconInfo), msg)
}

// Plain prints plain text without formatting
func (o *Output) Plain(format string, args ...interface{}) {
	fmt.Printf(format+"\n", args...)
}

// Bold prints bold text
func (o *Output) Bold(text string) string {
	return o.color(ColorBold, text)
}

// Dim prints dimmed text
func (o *Output) Dim(text string) string {
	return o.color(ColorDim, text)
}

// Table prints a simple table
func (o *Output) Table(headers []string, rows [][]string) {
	if len(rows) == 0 {
		return
	}

	// Calculate column widths
	colWidths := make([]int, len(headers))
	for i, header := range headers {
		colWidths[i] = len(header)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(colWidths) && len(cell) > colWidths[i] {
				colWidths[i] = len(cell)
			}
		}
	}

	// Print header
	headerStr := ""
	for i, header := range headers {
		headerStr += fmt.Sprintf("%-*s  ", colWidths[i], header)
	}
	fmt.Println(o.color(ColorBold, headerStr))

	// Print separator
	separator := strings.Repeat("─", len(headerStr))
	fmt.Println(separator)

	// Print rows
	for _, row := range rows {
		rowStr := ""
		for i, cell := range row {
			if i < len(colWidths) {
				rowStr += fmt.Sprintf("%-*s  ", colWidths[i], cell)
			}
		}
		fmt.Println(rowStr)
	}
}

// KeyValue prints a key-value pair
func (o *Output) KeyValue(key, value string) {
	fmt.Printf("  %s: %s\n", o.color(ColorBold, key), value)
}

// List prints a bulleted list
func (o *Output) List(items ...string) {
	for _, item := range items {
		fmt.Printf("  • %s\n", item)
	}
}

// NumberedList prints a numbered list
func (o *Output) NumberedList(items ...string) {
	for i, item := range items {
		fmt.Printf("  %d. %s\n", i+1, item)
	}
}

// Spinner prints a spinner message (not animated, just the icon)
func (o *Output) Spinner(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("⠋ %s\n", msg)
}

// Progress prints a progress message
func (o *Output) Progress(current, total int, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%d/%d] %s\n", current, total, msg)
}

// Divider prints a visual divider
func (o *Output) Divider() {
	fmt.Println(strings.Repeat("─", 60))
}

// EmptyLine prints an empty line
func (o *Output) EmptyLine() {
	fmt.Println()
}
