// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines application-wide constants.
package constants

import "time"

// HTTP header names and server timeout defaults
const (
	RequestIDHeader = "X-Request-ID"

	DefaultShutdownTimeout   = 25 * time.Second
	DefaultReadHeaderTimeout = 60 * time.Second
	DefaultWriteTimeout      = 60 * time.Second
	DefaultIdleTimeout       = 90 * time.Second

	// DefaultListenerDrainTimeout bounds how long shutdown waits for in-flight
	// event handlers before cancelling them.
	//
	// Spent before DefaultShutdownTimeout rather than alongside it, because the
	// listener is stopped first — a handler writes to Postgres and has to be
	// done with the pool before the pool is released. So the two add up, and
	// their sum plus a margin is what terminationGracePeriodSeconds has to
	// cover in the chart; changing either number without the chart is how a pod
	// starts being killed mid-drain.
	//
	// Ten seconds because that is the NATS request timeout a handler can be
	// waiting on. Unbounded was the previous behaviour and the wrong default
	// here: the drain exists to let a transaction finish, and a handler that
	// has not finished by now is not going to be helped by the kill that
	// arrives instead.
	DefaultListenerDrainTimeout = 10 * time.Second
)

type contextID int

const (
	// PrincipalContextID is the context ID for the principal (LFX username) from JWT claims
	PrincipalContextID contextID = iota
	// EmailContextID is the context ID for the email claim from the Heimdall JWT, populated by JWTAuth.
	EmailContextID
	// RequestIDContextID is the context ID for the per-request ID set by RequestIDMiddleware.
	RequestIDContextID
)
