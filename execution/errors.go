package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CleanupError means the supervisor could not confirm complete tree cleanup
// or release its OS resources. Both Wait and Close expose it via errors.As.
// Callers may retain resource reservations and alert/reconcile on this error.
type CleanupError struct{ Err error }

func (e *CleanupError) Error() string { return "execution: cleanup failed: " + e.Err.Error() }
func (e *CleanupError) Unwrap() error { return e.Err }

// Error codes carried by Error.Code.
const (
	CodeCanceled       = "canceled"
	CodeDeadline       = "deadline_exceeded"
	CodeMethodNotFound = "method_not_found"
	CodeBusy           = "busy"
	CodeInternal       = "internal"
)

var (
	// ErrClosed is returned by calls on (or pending at) a Peer that has shut down.
	// The shutdown cause, if any, is wrapped alongside it and is also in Peer.Err.
	ErrClosed = errors.New("execution: peer closed")
	// ErrFrameTooLarge: a frame exceeds MaxFrameBytes. Locally produced frames are
	// refused without side effects; an oversized incoming frame shuts the Peer down.
	ErrFrameTooLarge = errors.New("execution: frame too large")
	// ErrBacklog: undelivered stream events exceeded MaxBacklogBytes; the call is cancelled.
	ErrBacklog = errors.New("execution: stream backlog limit exceeded")
	// ErrProtocol: the remote violated the framing or message protocol.
	ErrProtocol = errors.New("execution: protocol violation")
	// ErrLimitsUnavailable: the requested resource limits cannot be enforced here
	// (unsupported platform, or no writable cgroup v2 subtree). Start never falls
	// back to running without the requested limits.
	ErrLimitsUnavailable = errors.New("execution: resource limits unavailable")

	// ErrMethodNotFound and ErrBusy match remote errors by code via errors.Is.
	ErrMethodNotFound = &Error{Code: CodeMethodNotFound}
	ErrBusy           = &Error{Code: CodeBusy}
)

// Error is the JSON form of an error crossing the wire. It satisfies
// errors.Is for context.Canceled / context.DeadlineExceeded (by code) and for
// the sentinel *Error values above.
type Error struct {
	Code    string          `json:"code"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *Error) Error() string {
	if e.Message == "" {
		return "execution: remote error: " + e.Code
	}
	return fmt.Sprintf("execution: remote error: %s: %s", e.Code, e.Message)
}

func (e *Error) Is(target error) bool {
	switch target {
	case context.Canceled:
		return e.Code == CodeCanceled
	case context.DeadlineExceeded:
		return e.Code == CodeDeadline
	}
	t, ok := target.(*Error)
	return ok && t.Message == "" && t.Data == nil && t.Code == e.Code
}

// MethodNotFound is what a Handler returns for a method it does not serve.
func MethodNotFound(method string) *Error {
	return &Error{Code: CodeMethodNotFound, Message: "unknown method " + method}
}

// toError converts any handler error to its wire form.
func toError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		if err == error(e) {
			return e
		}
		return &Error{Code: e.Code, Message: err.Error(), Data: e.Data}
	case errors.Is(err, context.Canceled):
		return &Error{Code: CodeCanceled, Message: err.Error()}
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeDeadline, Message: err.Error()}
	}
	return &Error{Code: CodeInternal, Message: err.Error()}
}
