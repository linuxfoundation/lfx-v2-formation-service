// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines application-wide constants.
package constants

// Environment variable names
const (
	EnvPort  = "PORT"
	EnvHost  = "HOST"
	EnvDebug = "DEBUG"

	EnvJWKSURL  = "JWKS_URL"
	EnvAudience = "AUDIENCE"
	EnvIssuer   = "ISSUER"

	EnvNATSURL = "NATS_URL"
)

// Default values
const (
	DefaultHost     = "0.0.0.0"
	DefaultHTTPPort = "8080"

	DefaultJWKSURL  = "http://heimdall:4457/.well-known/jwks"
	DefaultAudience = "lfx-v2-formation-service"
	DefaultIssuer   = "heimdall"

	DefaultNATSURL = "nats://nats:4222"
)
