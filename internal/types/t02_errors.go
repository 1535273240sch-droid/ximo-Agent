// Package types holds the public data contracts shared by the Engine, the
// Agent loop and the Scheduler.
//
// Ownership note (task 02): the architecture document (ch. 32) leaves these
// types to be reconciled by task 08 at integration time. Task 02 is the
// primary consumer of RunState/Event/Task, so the definitions here are the
// ones registered in "需要主Agent裁决的公共类型" in the task book. Any field
// name that has to change during integration should be changed here once and
// then propagated, rather than redefined per package.
package types

import (
	"errors"
	"fmt"
)

// ErrorCode is a stable, machine-readable error identifier. Codes cross the
// IPC boundary (task 01) and reach the UI host, so they are part of the wire
// contract: append new codes, never rename an existing one.
type ErrorCode string

const (
	CodeInternal            ErrorCode = "internal"
	CodeInvalidArgument     ErrorCode = "invalid_argument"
	CodeNotFound            ErrorCode = "not_found"
	CodeAlreadyExists       ErrorCode = "already_exists"
	CodeInvalidTransition   ErrorCode = "invalid_transition"
	CodeTerminalState       ErrorCode = "terminal_state"
	CodeQueueFull           ErrorCode = "queue_full"
	CodeAdmissionRejected   ErrorCode = "admission_rejected"
	CodeResourceUnavailable ErrorCode = "resource_unavailable"
	CodeBudgetExceeded      ErrorCode = "budget_exceeded"
	CodeRunDurationExceeded ErrorCode = "run_duration_exceeded"
	CodeOutputLimitExceeded ErrorCode = "output_limit_exceeded"
	CodeMemoryLimitExceeded ErrorCode = "memory_limit_exceeded"
	CodeCancelled           ErrorCode = "cancelled"
	CodeDeadlineExceeded    ErrorCode = "deadline_exceeded"
	CodeEngineClosed        ErrorCode = "engine_closed"
	CodeActorClosed         ErrorCode = "actor_closed"
	CodeCorruptEventLog     ErrorCode = "corrupt_event_log"
	CodeNotResumable        ErrorCode = "not_resumable"
	CodeAwaitingUser        ErrorCode = "awaiting_user"
	CodeProviderFailed      ErrorCode = "provider_failed"
	CodeToolFailed          ErrorCode = "tool_failed"
	CodeToolTimeout         ErrorCode = "tool_timeout"
	CodeCrash               ErrorCode = "crash"
	CodePanicIsolated       ErrorCode = "panic_isolated"
)

// Error is the single error type carried across package boundaries. Code is
// the machine-readable part; Msg is safe for display, which means it must
// already be redacted (see RedactString) before it is placed here.
type Error struct {
	Code ErrorCode
	Msg  string
	Err  error
}

func (e *Error) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Msg, e.Err)
	case e.Msg != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Msg)
	case e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	default:
		return string(e.Code)
	}
}

func (e *Error) Unwrap() error { return e.Err }

// Is reports a match when the target carries the same ErrorCode. This makes
// the sentinel values below usable with errors.Is even though they are
// constructed with distinct messages.
func (e *Error) Is(target error) bool {
	var t *Error
	if errors.As(target, &t) {
		return t.Code == e.Code
	}
	return false
}

// NewError builds a coded error. Message formatting arguments are redacted so
// that a stray API key can never reach a log line or an event payload
// (architecture doc ch. 21).
func NewError(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Msg: RedactString(fmt.Sprintf(format, args...))}
}

// WrapError attaches a code to an existing error, redacting its text.
func WrapError(code ErrorCode, err error, format string, args ...any) *Error {
	if err == nil {
		return nil
	}
	return &Error{Code: code, Msg: RedactString(fmt.Sprintf(format, args...)), Err: err}
}

// Sentinel errors. Compare with errors.Is; the message is only a fallback for
// callers that print the error directly.
var (
	ErrInternal            = &Error{Code: CodeInternal, Msg: "internal error"}
	ErrInvalidArgument     = &Error{Code: CodeInvalidArgument, Msg: "invalid argument"}
	ErrNotFound            = &Error{Code: CodeNotFound, Msg: "not found"}
	ErrAlreadyExists       = &Error{Code: CodeAlreadyExists, Msg: "already exists"}
	ErrInvalidTransition   = &Error{Code: CodeInvalidTransition, Msg: "invalid state transition"}
	ErrTerminalState       = &Error{Code: CodeTerminalState, Msg: "run is in a terminal state"}
	ErrQueueFull           = &Error{Code: CodeQueueFull, Msg: "queue is full"}
	ErrAdmissionRejected   = &Error{Code: CodeAdmissionRejected, Msg: "admission rejected"}
	ErrResourceUnavailable = &Error{Code: CodeResourceUnavailable, Msg: "resource unavailable"}
	ErrBudgetExceeded      = &Error{Code: CodeBudgetExceeded, Msg: "budget exceeded"}
	ErrRunDurationExceeded = &Error{Code: CodeRunDurationExceeded, Msg: "run duration exceeded"}
	ErrOutputLimitExceeded = &Error{Code: CodeOutputLimitExceeded, Msg: "output limit exceeded"}
	ErrMemoryLimitExceeded = &Error{Code: CodeMemoryLimitExceeded, Msg: "memory limit exceeded"}
	ErrCancelled           = &Error{Code: CodeCancelled, Msg: "cancelled"}
	ErrDeadlineExceeded    = &Error{Code: CodeDeadlineExceeded, Msg: "deadline exceeded"}
	ErrEngineClosed        = &Error{Code: CodeEngineClosed, Msg: "engine is closed"}
	ErrActorClosed         = &Error{Code: CodeActorClosed, Msg: "session actor is closed"}
	ErrCorruptEventLog     = &Error{Code: CodeCorruptEventLog, Msg: "event log is corrupt"}
	ErrNotResumable        = &Error{Code: CodeNotResumable, Msg: "run is not resumable"}
	ErrAwaitingUser        = &Error{Code: CodeAwaitingUser, Msg: "run is waiting for user input"}
	ErrProviderFailed      = &Error{Code: CodeProviderFailed, Msg: "provider call failed"}
	ErrToolFailed          = &Error{Code: CodeToolFailed, Msg: "tool execution failed"}
	ErrToolTimeout         = &Error{Code: CodeToolTimeout, Msg: "tool execution timed out"}
	ErrCrash               = &Error{Code: CodeCrash, Msg: "engine core crashed"}
)

// CodeOf extracts the ErrorCode from an error chain, defaulting to
// CodeInternal for foreign errors.
func CodeOf(err error) ErrorCode {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// IsCancelled reports whether err represents a user or context cancellation.
func IsCancelled(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrCancelled) || errors.Is(err, ErrDeadlineExceeded)
}
