// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package constants defines application-wide constants.
package constants

import "time"

// Environment variable names
const (
	EnvPort  = "PORT"
	EnvHost  = "HOST"
	EnvDebug = "DEBUG"

	EnvJWKSURL  = "JWKS_URL"
	EnvAudience = "AUDIENCE"
	EnvIssuer   = "ISSUER"

	EnvNATSURL = "NATS_URL"

	// Database credentials arrive as five discrete values, PGHOST/PGPORT/
	// PGUSER/PGPASSWORD/PGDATABASE — the libpq-standard names the chart's
	// deployment.yaml injects (see lfx-v2-newsletter-service's
	// composeDatabaseURL, the precedent for this exact env-var set). There
	// is deliberately no single-URL form: composing the DSN in process
	// keeps the password out of any value that gets logged whole.
	EnvDBHost     = "PGHOST"
	EnvDBPort     = "PGPORT"
	EnvDBUsername = "PGUSER"
	EnvDBPassword = "PGPASSWORD"
	EnvDBName     = "PGDATABASE"
	// EnvDBSSLMode is not part of the newsletter precedent, but PGSSLMODE
	// is libpq's own standard name, so an override here needs no new
	// convention — only a local dev container without TLS needs to set it.
	EnvDBSSLMode = "PGSSLMODE"

	EnvReconcileInterval = "RECONCILE_INTERVAL"

	// EnvJWTMockBypassConfirm must be set to "true" alongside
	// JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL for the latter to take effect
	// under AUTH_SOURCE=jwt. Requiring two independent env vars means a
	// single stray value (a leftover app.extraEnv entry, a copy-pasted
	// ArgoCD values override) cannot silently authenticate every request
	// as a fixed principal; both must be set deliberately.
	EnvJWTMockBypassConfirm = "JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL_CONFIRM"
)

// Default values
const (
	DefaultHost     = "0.0.0.0"
	DefaultHTTPPort = "8080"

	DefaultJWKSURL  = "http://heimdall:4457/.well-known/jwks"
	DefaultAudience = "lfx-v2-formation-service"
	DefaultIssuer   = "heimdall"

	DefaultNATSURL = "nats://nats:4222"

	DefaultDBHost = "localhost"
	DefaultDBPort = "5432"
	DefaultDBName = "formation"
	// DefaultDBSSLMode is empty, matching the newsletter precedent: pgx's own
	// default (prefer) upgrades opportunistically without a hardcoded
	// requirement, and a local dev container without TLS needs no override.
	DefaultDBSSLMode = ""

	// DefaultReconcileInterval is the period of the reconcile ticker. The
	// loop is the primary correctness path, so this bounds how long a
	// missed change notification can leave a project without a checklist.
	DefaultReconcileInterval = 15 * time.Minute
)
