// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package mock

import (
	"context"
	"errors"
	"log/slog"
	"os"
)

// AuthService is an in-memory port.Authenticator double for local development
// and tests. It ignores the token entirely, so it is only ever reachable
// behind an explicit AUTH_SOURCE=mock.
type AuthService struct{}

// NewAuthService constructs the double.
func NewAuthService() *AuthService {
	return &AuthService{}
}

// ParsePrincipal returns a mock principal and email from environment variables (ignores token parameter).
// Set JWT_AUTH_DISABLED_MOCK_LOCAL_EMAIL to simulate a JWT email claim in local dev.
// An unset principal is an error rather than an anonymous success, so a
// half-configured mock cannot become an unauthenticated allow.
func (m *AuthService) ParsePrincipal(ctx context.Context, _ string, logger *slog.Logger) (string, string, error) {
	principal := os.Getenv("JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL")
	if principal == "" {
		return "", "", errors.New("mock principal not configured in JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL")
	}

	email := os.Getenv("JWT_AUTH_DISABLED_MOCK_LOCAL_EMAIL")

	logger.DebugContext(ctx, "parsed principal",
		"user_id", principal,
	)

	return principal, email, nil
}
