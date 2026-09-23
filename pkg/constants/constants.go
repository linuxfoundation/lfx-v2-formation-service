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

	// EnvFormationInboxEmail is the address that receives formation-team
	// notifications (Activating / announcement reminders). Defaults to
	// DefaultFormationInboxEmail.
	EnvFormationInboxEmail = "FORMATION_INBOX_EMAIL"

	// EnvFormationAdminBaseURL is the base URL for the formation admin tool
	// (used to build deep links in outbound emails). Defaults to
	// DefaultFormationAdminBaseURL.
	EnvFormationAdminBaseURL = "FORMATION_ADMIN_BASE_URL"

	// EnvApplicationFormationTeam names the OpenFGA team granted review
	// standing on every project application this service creates.
	//
	// It reaches the tuple store, not just a gateway rule: the grant is
	// written as `team:<this>#member`, so a value that names no real team
	// produces a tuple nobody satisfies and a review queue that is empty for
	// everyone. Defaults to DefaultApplicationFormationTeam.
	EnvApplicationFormationTeam = "APPLICATION_FORMATION_TEAM"

	// EnvEmailEnabled mirrors the email-service's own flag so callers can
	// gate dispatches without standing up a real NATS broker in tests.
	// "true" / "1" / "t" enables; anything else (including unset) disables.
	EnvEmailEnabled = "EMAIL_ENABLED"

	// EnvJWTMockBypassConfirm must be set to "true" alongside
	// JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL for the latter to take effect
	// under AUTH_SOURCE=jwt. Requiring two independent env vars means a
	// single stray value (a leftover app.extraEnv entry, a copy-pasted
	// ArgoCD values override) cannot silently authenticate every request
	// as a fixed principal; both must be set deliberately.
	EnvJWTMockBypassConfirm = "JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL_CONFIRM"

	// EnvQueryServiceURL is the base URL of the shared read layer the
	// platform checks resolve against. Unset is a supported, and the
	// default, state: the lookup registry is then built empty and every
	// platform row stays unanswerable exactly as it did before, which is
	// the correct fallback because the alternative is rows failing rather
	// than going unanswered. Enablement is per environment through the
	// values hierarchy, never through a code change.
	EnvQueryServiceURL = "QUERY_SERVICE_URL"

	// The service identity's credentials. Outbound reads are made as this
	// service rather than as the caller whose request happened to trigger
	// the sweep — a sweep has no caller at all, so there is no bearer token
	// to forward even in principle.
	//
	// These are the names lfx-v2-committee-service already uses for the
	// same purpose against the same read layer; the flow is an Auth0
	// client-credentials grant with a private-key JWT assertion (RS256),
	// not a client secret. Reusing the names means one deployment-time
	// convention across services instead of two.
	EnvM2MClientID   = "M2M_AUTH_CLIENT_ID"
	EnvM2MPrivateKey = "M2M_AUTH_PRIVATE_KEY"
	EnvM2MDomain     = "M2M_AUTH_DOMAIN"
	EnvM2MAudience   = "M2M_AUTH_AUDIENCE"
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
	// DefaultDBSSLMode is "require" so a deployed pod always mandates TLS:
	// the chart never sets PGSSLMODE for any deploy mode, and pgx's own
	// default (prefer) would otherwise fall back to an unencrypted
	// connection on any TLS handshake failure instead of refusing it. Local
	// dev overrides this explicitly with PGSSLMODE=disable.
	DefaultDBSSLMode = "require"

	// DefaultFormationInboxEmail is the address that receives formation-team
	// notifications when FORMATION_INBOX_EMAIL is not set.
	DefaultFormationInboxEmail = "formation@linuxfoundation.org"

	// DefaultApplicationFormationTeam is the team granted review standing on
	// applications when APPLICATION_FORMATION_TEAM is not set.
	//
	// Deliberately not the same team as the chart's applicationIntakeTeamName,
	// which guards the create route. That one holds platform-wide create
	// authority; this one is the formation staff who work the queue. One team
	// for both would let the reviewing team create
	// applications naming anyone as the applicant.
	DefaultApplicationFormationTeam = "formation"

	// DefaultFormationAdminBaseURL is the base URL used to build checklist deep
	// links in formation emails when FORMATION_ADMIN_BASE_URL is not set.
	// Links resolve to: <base>/foundation/formations/<formation-slug>?project=<parent-slug>.
	DefaultFormationAdminBaseURL = "https://app.lfx.dev"

	// DefaultReconcileInterval is the period of the reconcile ticker. The
	// loop remains the only mechanism guaranteed to run, so this bounds how
	// long a missed project event can leave a project without a checklist —
	// but it is no longer how long that normally takes, because the listener
	// reconciles on the change itself. Daily rather than quarter-hourly: the
	// sweep reads every forming project on every replica, and paying that
	// ninety-six times a day to shorten a window the listener already closes
	// is load spent against the project service for nothing.
	DefaultReconcileInterval = 24 * time.Hour

	// DefaultQueryServiceTimeout bounds one outbound lookup. A sweep runs a
	// lookup per platform row per project, so a read layer that has stopped
	// answering must fail the row quickly rather than hold the sweep open;
	// a row left unresolved is retried by the next sweep, which is the only
	// retry this feature has by design.
	DefaultQueryServiceTimeout = 15 * time.Second
)
