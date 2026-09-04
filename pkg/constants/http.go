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
)
