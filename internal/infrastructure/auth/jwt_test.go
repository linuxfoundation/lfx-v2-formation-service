// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/auth0/go-jwt-middleware/v2/validator"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	jose "gopkg.in/go-jose/go-jose.v2"
	"gopkg.in/go-jose/go-jose.v2/jwt"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

// signHeimdallToken builds and HS256-signs a JWT carrying the given
// HeimdallClaims, for use against a validator constructed with a matching
// HMAC keyFunc. HS256 rather than production's PS256 keeps these tests free
// of a JWKS server or RSA key pair while still exercising the real
// validator.Validator path (issuer, audience, algorithm, custom claims).
func signHeimdallToken(t *testing.T, secret []byte, issuer, audience string, claims HeimdallClaims) string {
	t.Helper()

	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: secret}, nil)
	require.NoError(t, err)

	token, err := jwt.Signed(signer).
		Claims(jwt.Claims{Issuer: issuer, Audience: jwt.Audience{audience}}).
		Claims(claims).
		CompactSerialize()
	require.NoError(t, err)

	return token
}

// newTestJWTAuth builds a JWTAuth around a real validator.Validator keyed on
// secret, so tests exercise ParsePrincipal's real-token path rather than the
// mock-principal shortcut.
func newTestJWTAuth(t *testing.T, secret []byte, issuer, audience string) *JWTAuth {
	t.Helper()

	v, err := validator.New(
		func(context.Context) (interface{}, error) { return secret, nil },
		validator.HS256,
		issuer,
		[]string{audience},
		validator.WithCustomClaims(customClaims),
	)
	require.NoError(t, err)

	return &JWTAuth{validator: v, config: JWTAuthConfig{Issuer: issuer, Audience: audience}}
}

func TestParsePrincipal_RealValidator(t *testing.T) {
	const (
		issuer   = "test-issuer"
		audience = "test-audience"
	)
	secret := []byte("test-secret")
	auth := newTestJWTAuth(t, secret, issuer, audience)

	t.Run("valid Heimdall claim returns principal and email", func(t *testing.T) {
		token := signHeimdallToken(t, secret, issuer, audience, HeimdallClaims{Principal: "user123", Email: "user123@example.com"})

		principal, email, err := auth.ParsePrincipal(context.Background(), token, slog.Default())

		require.NoError(t, err)
		assert.Equal(t, "user123", principal)
		assert.Equal(t, "user123@example.com", email)
	})

	t.Run("valid claim without email returns empty email", func(t *testing.T) {
		token := signHeimdallToken(t, secret, issuer, audience, HeimdallClaims{Principal: "user123"})

		principal, email, err := auth.ParsePrincipal(context.Background(), token, slog.Default())

		require.NoError(t, err)
		assert.Equal(t, "user123", principal)
		assert.Empty(t, email)
	})

	t.Run("missing principal is rejected", func(t *testing.T) {
		token := signHeimdallToken(t, secret, issuer, audience, HeimdallClaims{Email: "user123@example.com"})

		_, _, err := auth.ParsePrincipal(context.Background(), token, slog.Default())

		assert.Error(t, err)
	})

	t.Run("wrong issuer is rejected", func(t *testing.T) {
		token := signHeimdallToken(t, secret, "wrong-issuer", audience, HeimdallClaims{Principal: "user123"})

		_, _, err := auth.ParsePrincipal(context.Background(), token, slog.Default())

		assert.Error(t, err)
	})

	t.Run("wrong audience is rejected", func(t *testing.T) {
		token := signHeimdallToken(t, secret, issuer, "wrong-audience", HeimdallClaims{Principal: "user123"})

		_, _, err := auth.ParsePrincipal(context.Background(), token, slog.Default())

		assert.Error(t, err)
	})

	t.Run("wrong signing key is rejected", func(t *testing.T) {
		token := signHeimdallToken(t, []byte("a-different-secret"), issuer, audience, HeimdallClaims{Principal: "user123"})

		_, _, err := auth.ParsePrincipal(context.Background(), token, slog.Default())

		assert.Error(t, err)
	})

	t.Run("wrong signing algorithm is rejected", func(t *testing.T) {
		signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS384, Key: secret}, nil)
		require.NoError(t, err)
		token, err := jwt.Signed(signer).
			Claims(jwt.Claims{Issuer: issuer, Audience: jwt.Audience{audience}}).
			Claims(HeimdallClaims{Principal: "user123"}).
			CompactSerialize()
		require.NoError(t, err)

		_, _, err = auth.ParsePrincipal(context.Background(), token, slog.Default())

		assert.Error(t, err)
	})

	t.Run("malformed token is rejected", func(t *testing.T) {
		_, _, err := auth.ParsePrincipal(context.Background(), "not-a-jwt", slog.Default())

		assert.Error(t, err)
	})
}

func TestParsePrincipal_KeyProviderFailure(t *testing.T) {
	// A validator whose keyFunc always fails with the domain sentinel,
	// mirroring how NewJWTAuth wraps the JWKS provider's KeyFunc in
	// production: the key-provider error must reach ParsePrincipal as
	// domain.ErrAuthUnavailable, not the sanitized generic error every other
	// validation failure gets.
	v, err := validator.New(
		func(context.Context) (interface{}, error) {
			return nil, fmt.Errorf("%w: %w", domain.ErrAuthUnavailable, errors.New("JWKS endpoint unreachable"))
		},
		validator.HS256,
		"issuer",
		[]string{"audience"},
		validator.WithCustomClaims(customClaims),
	)
	require.NoError(t, err)
	auth := &JWTAuth{validator: v}

	token := signHeimdallToken(t, []byte("irrelevant"), "issuer", "audience", HeimdallClaims{Principal: "user123"})

	_, _, err = auth.ParsePrincipal(context.Background(), token, slog.Default())

	assert.ErrorIs(t, err, domain.ErrAuthUnavailable)
}
