package domain

import "errors"

// Sentinel errors of the domain; check with errors.Is.
var (
	// ErrNotFound means the requested entity does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidTransition means the clip cannot move to the requested state.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrExpired means the approval window of a clip has passed.
	ErrExpired = errors.New("approval expired")
	// ErrInvalidEditPlan means the editor output failed validation.
	ErrInvalidEditPlan = errors.New("invalid edit plan")
	// ErrWindowUnavailable means the requested media window is no longer (or
	// not yet) in the capture buffer.
	ErrWindowUnavailable = errors.New("media window unavailable")
	// ErrInvalidArgument means a constructor received inconsistent input.
	ErrInvalidArgument = errors.New("invalid argument")
)
