package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/redentordev/tako-cli/pkg/deployer"
	"github.com/redentordev/tako-cli/pkg/takodclient"
)

// Class categorizes engine errors for machine consumers. The CLI maps
// classes to process exit codes; see docs/MACHINE-INTERFACE.md.
type Class int

const (
	// ClassNone means no error.
	ClassNone Class = 0
	// ClassFailed is an operation that ran and failed (deploy error).
	ClassFailed Class = 1
	// ClassInvalid is a rejected request: bad flags, config, or state.
	ClassInvalid Class = 2
	// ClassLocked means a concurrent operation holds the lock or lease.
	ClassLocked Class = 3
	// ClassConnectivity is an SSH or node connectivity failure.
	ClassConnectivity Class = 4
	// ClassCancelled means the context was cancelled mid-operation.
	ClassCancelled Class = 5
	// ClassAttention is a partial success needing operator attention.
	ClassAttention Class = 6
)

// InvalidRequestError rejects a request before any mutation happens.
type InvalidRequestError struct {
	Err error
}

func (e *InvalidRequestError) Error() string { return e.Err.Error() }
func (e *InvalidRequestError) Unwrap() error { return e.Err }

func invalidRequestf(format string, args ...any) error {
	return &InvalidRequestError{Err: fmt.Errorf(format, args...)}
}

// LockedError means the local state lock or a remote lease is held by
// another operation. Holder describes the owner when known.
type LockedError struct {
	Operation string
	Holder    string
	Err       error
}

func (e *LockedError) Error() string { return e.Err.Error() }
func (e *LockedError) Unwrap() error { return e.Err }

// ConnectivityError wraps SSH/node connection failures.
type ConnectivityError struct {
	Server string
	Err    error
}

func (e *ConnectivityError) Error() string { return e.Err.Error() }
func (e *ConnectivityError) Unwrap() error { return e.Err }

// ConfirmationRequiredError is returned by non-interactive apply paths when
// the computed plan needs explicit approval.
type ConfirmationRequiredError struct {
	Reason string
}

func (e *ConfirmationRequiredError) Error() string {
	return fmt.Sprintf("%s; rerun with --yes to approve in non-interactive mode", e.Reason)
}

// AttentionError marks a partial success that needs operator follow-up.
type AttentionError struct {
	Err error
}

func (e *AttentionError) Error() string { return e.Err.Error() }
func (e *AttentionError) Unwrap() error { return e.Err }

// HookFailedError is a local lifecycle hook that blocked an operation: a
// pre-* hook that exited non-zero, could not start, or timed out. It is an
// operation that ran and failed by policy (exit code 1); a hook stopped by
// cancellation wraps the context error and classifies as cancelled.
type HookFailedError struct {
	Hook     string
	ExitCode *int
	TimedOut bool
	Err      error
}

func (e *HookFailedError) Error() string { return e.Err.Error() }
func (e *HookFailedError) Unwrap() error { return e.Err }

// BeforeMutationError marks an apply error raised before anything was
// changed (preflight and validation failures), so a caller that has no result
// can report the operation as failed with nothing attempted instead of an
// unknown partial state. It does not change the error's Class.
type BeforeMutationError struct {
	Err error
}

func (e *BeforeMutationError) Error() string { return e.Err.Error() }
func (e *BeforeMutationError) Unwrap() error { return e.Err }

// beforeMutation wraps err as a BeforeMutationError; nil stays nil.
func beforeMutation(err error) error {
	if err == nil {
		return nil
	}
	return &BeforeMutationError{Err: err}
}

// IsBeforeMutation reports whether err was raised before any change.
func IsBeforeMutation(err error) bool {
	var target *BeforeMutationError
	return errors.As(err, &target)
}

// Classify maps an error to its Class for exit-code selection.
func Classify(err error) Class {
	if err == nil {
		return ClassNone
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassCancelled
	}
	var invalid *InvalidRequestError
	if errors.As(err, &invalid) {
		return ClassInvalid
	}
	var confirmation *ConfirmationRequiredError
	if errors.As(err, &confirmation) {
		return ClassInvalid
	}
	var capability *takodclient.CapabilityRequiredError
	if errors.As(err, &capability) {
		return ClassInvalid
	}
	var unappliedMesh *deployer.MeshAddressUnappliedError
	if errors.As(err, &unappliedMesh) {
		return ClassInvalid
	}
	var locked *LockedError
	if errors.As(err, &locked) {
		return ClassLocked
	}
	var connectivity *ConnectivityError
	if errors.As(err, &connectivity) {
		return ClassConnectivity
	}
	var attention *AttentionError
	if errors.As(err, &attention) {
		return ClassAttention
	}
	// Everything else, including HookFailedError, is an operation that ran
	// and failed. An AttentionError wrapping a failed post hook stays
	// attention-class because it is matched above.
	return ClassFailed
}
