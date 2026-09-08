// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package domain holds the errors the service layer maps onto transport
// responses.
package domain

import "errors"

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
