package cmd

import (
	"errors"
	"fmt"
)

type exitCodeError struct {
	code int
	err  error
}

func newExitCodeError(code int, err error) error {
	if code < 0 {
		code = 1
	}
	if code > 255 {
		code = 255
	}
	return exitCodeError{code: code, err: err}
}

func (e exitCodeError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("command exited with status %d", e.code)
}

func (e exitCodeError) Unwrap() error {
	return e.err
}

func commandExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var exitErr exitCodeError
	if errors.As(err, &exitErr) {
		return exitErr.code, true
	}
	return 0, false
}
