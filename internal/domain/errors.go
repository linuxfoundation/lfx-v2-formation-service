// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package domain holds the errors the service layer maps onto transport
// responses.
package domain

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound is returned when a formation, item or template does not
	// exist. Maps to 404.
	ErrNotFound = errors.New("not found")

	// ErrVersionMismatch means the caller's copy was stale: the row moved
	// under them. Maps to 412 with version_mismatch, deliberately not 409 —
	// "re-read and retry" is a different instruction from "this cannot be
	// done in the current state".
	ErrVersionMismatch = errors.New("version mismatch")

	// ErrAlreadyExists means a formation already exists for the project.
	// Creation treats this as success, since the uniqueness constraint is
	// how concurrent replicas are made safe rather than an error path.
	ErrAlreadyExists = errors.New("already exists")

	// ErrConflict is a refusal on domain state rather than on staleness:
	// accepting an already-accepted item, or mutating a frozen checklist.
	// Maps to 409.
	ErrConflict = errors.New("conflict")

	// ErrInvalidRequest covers validation failures, including a skip with
	// no reason and a link whose scheme is not http or https. Maps to 400.
	ErrInvalidRequest = errors.New("invalid request")

	// ErrForbidden is returned when the caller may read the checklist but
	// not perform this particular change. Maps to 403.
	ErrForbidden = errors.New("forbidden")

	// ErrAuthUnavailable means the JWT could not be validated because the
	// key provider (Heimdall's JWKS endpoint) could not be reached, not
	// because the token itself is invalid. It is returned as a plain error
	// rather than a declared one, so Goa's default formatter encodes it as
	// 500 — the point is only that it is not the 401 a bad or expired token
	// gets, because a JWKS outage is a server-side failure and must not tell
	// a valid caller their credentials are bad.
	ErrAuthUnavailable = errors.New("authentication service unavailable")
)

// ReasonError pairs one of the sentinels above with a machine-readable
// reason string. One HTTP status can carry several distinct reasons (both
// checklist_read_only and invalid_transition are 409, for instance), so the
// sentinel alone is not enough for the transport layer to fill in the wire
// error's reason field. errors.Is still works against the sentinel through
// Unwrap; callers that only care about the status keep using errors.Is
// exactly as before.
type ReasonError struct {
	Err error
	// Reason is the machine-readable value the UI should switch on.
	Reason string
	// Message, when set, overrides the transport layer's generic message
	// for Reason — for naming the specific item key an "unknown item key"
	// refusal was about, for instance, which no reason-keyed static string
	// can do.
	Message string
}

// NewReasonError constructs a ReasonError with the reason's default message.
func NewReasonError(err error, reason string) *ReasonError {
	return &ReasonError{Err: err, Reason: reason}
}

// NewReasonErrorf constructs a ReasonError with a message formatted for this
// occurrence, overriding the reason's default.
func NewReasonErrorf(err error, reason, format string, args ...any) *ReasonError {
	return &ReasonError{Err: err, Reason: reason, Message: fmt.Sprintf(format, args...)}
}

func (e *ReasonError) Error() string { return e.Err.Error() + ": " + e.Reason }

func (e *ReasonError) Unwrap() error { return e.Err }
