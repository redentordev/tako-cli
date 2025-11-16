package utils

import (
	"fmt"
	"strings"
)

// Error represents a Tako CLI error with context
type Error struct {
	Operation string   // What operation was being performed
	Cause     error    // The underlying error
	Details   []string // Additional context
}

// Error implements the error interface
func (e *Error) Error() string {
	var parts []string

	if e.Operation != "" {
		parts = append(parts, e.Operation)
	}

	if e.Cause != nil {
		parts = append(parts, e.Cause.Error())
	}

	if len(e.Details) > 0 {
		parts = append(parts, strings.Join(e.Details, "; "))
	}

	return strings.Join(parts, ": ")
}

// Unwrap returns the underlying error
func (e *Error) Unwrap() error {
	return e.Cause
}

// NewError creates a new Tako CLI error
func NewError(operation string, cause error, details ...string) *Error {
	return &Error{
		Operation: operation,
		Cause:     cause,
		Details:   details,
	}
}

// Wrapf wraps an error with a formatted message
func Wrapf(err error, format string, args ...interface{}) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}

// Errorf creates a formatted error
func Errorf(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

// Multi error type for collecting multiple errors
type MultiError struct {
	Errors []error
}

// Error implements the error interface
func (m *MultiError) Error() string {
	if len(m.Errors) == 0 {
		return "no errors"
	}
	if len(m.Errors) == 1 {
		return m.Errors[0].Error()
	}

	var messages []string
	for i, err := range m.Errors {
		messages = append(messages, fmt.Sprintf("%d. %s", i+1, err.Error()))
	}
	return fmt.Sprintf("multiple errors occurred:\n%s", strings.Join(messages, "\n"))
}

// Add adds an error to the collection
func (m *MultiError) Add(err error) {
	if err != nil {
		m.Errors = append(m.Errors, err)
	}
}

// HasErrors returns true if there are any errors
func (m *MultiError) HasErrors() bool {
	return len(m.Errors) > 0
}

// ErrorOrNil returns the error if there are any, otherwise nil
func (m *MultiError) ErrorOrNil() error {
	if m.HasErrors() {
		return m
	}
	return nil
}
