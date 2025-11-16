package secrets

import (
	"fmt"
	"io"
	"os"
)

// SafeOutput wraps output functions to automatically redact sensitive data
type SafeOutput struct {
	redactor *Redactor
	writer   io.Writer
}

// NewSafeOutput creates a new safe output wrapper
func NewSafeOutput(redactor *Redactor) *SafeOutput {
	return &SafeOutput{
		redactor: redactor,
		writer:   os.Stdout,
	}
}

// SetWriter sets the output writer (for testing)
func (so *SafeOutput) SetWriter(w io.Writer) {
	so.writer = w
}

// Printf formats and prints redacted output
func (so *SafeOutput) Printf(format string, args ...interface{}) {
	output := fmt.Sprintf(format, args...)
	redacted := so.redactor.Redact(output)
	fmt.Fprint(so.writer, redacted)
}

// Println prints redacted output with newline
func (so *SafeOutput) Println(args ...interface{}) {
	output := fmt.Sprint(args...)
	redacted := so.redactor.Redact(output)
	fmt.Fprintln(so.writer, redacted)
}

// Print prints redacted output
func (so *SafeOutput) Print(args ...interface{}) {
	output := fmt.Sprint(args...)
	redacted := so.redactor.Redact(output)
	fmt.Fprint(so.writer, redacted)
}

// Error creates a redacted error
func (so *SafeOutput) Error(err error) error {
	if err == nil {
		return nil
	}

	redactedMsg := so.redactor.Redact(err.Error())
	return fmt.Errorf("%s", redactedMsg)
}

// Errorf creates a formatted redacted error
func (so *SafeOutput) Errorf(format string, args ...interface{}) error {
	msg := fmt.Sprintf(format, args...)
	redactedMsg := so.redactor.Redact(msg)
	return fmt.Errorf("%s", redactedMsg)
}

// Success prints a success message with checkmark
func (so *SafeOutput) Success(format string, args ...interface{}) {
	so.Printf("✓ "+format+"\n", args...)
}

// Info prints an info message
func (so *SafeOutput) Info(format string, args ...interface{}) {
	so.Printf("→ "+format+"\n", args...)
}

// Warning prints a warning message
func (so *SafeOutput) Warning(format string, args ...interface{}) {
	so.Printf("⚠ "+format+"\n", args...)
}

// Debug prints a debug message (only if verbose mode)
func (so *SafeOutput) Debug(verbose bool, format string, args ...interface{}) {
	if verbose {
		so.Printf("  [DEBUG] "+format+"\n", args...)
	}
}
