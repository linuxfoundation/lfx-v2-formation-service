// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package auth validates Heimdall-issued JWTs against Heimdall's JWKS
// endpoint and extracts the principal and email claims.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/auth0/go-jwt-middleware/v2/jwks"
	"github.com/auth0/go-jwt-middleware/v2/validator"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

const (
	// PS256 is the default for Heimdall's JWT finalizer.
	signatureAlgorithm = validator.PS256
	defaultIssuer      = constants.DefaultIssuer
	defaultAudience    = constants.DefaultAudience
	defaultJWKSURL     = constants.DefaultJWKSURL
	jwksClientTimeout  = 10 * time.Second
)

// JWTAuthConfig holds the configuration parameters for JWT authentication.
type JWTAuthConfig struct {
	// JWKSURL is the URL to the JSON Web Key Set endpoint
	JWKSURL string
	// Audience is the intended audience for the JWT token
	Audience string
	// Issuer is the expected iss claim value
	Issuer string
	// MockLocalPrincipal, when set via JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL, is returned
	// instead of parsing the JWT principal.
	MockLocalPrincipal string
	// MockLocalEmail, when set via JWT_AUTH_DISABLED_MOCK_LOCAL_EMAIL, is returned
	// with the mock principal.
	MockLocalEmail string
}

var (
	// Factory for custom JWT claims target.
	customClaims = func() validator.CustomClaims {
		return &HeimdallClaims{}
	}
)

// HeimdallClaims contains extra custom claims we want to parse from the JWT
// token.
type HeimdallClaims struct {
	Principal string `json:"principal"`
	Email     string `json:"email"`
}

// Validate provides additional middleware validation of any claims defined in
// HeimdallClaims.
func (c *HeimdallClaims) Validate(_ context.Context) error {
	if c.Principal == "" {
		return errors.New("principal must be provided")
	}
	return nil
}

// JWTAuth provides JWT token validation and principal extraction using Heimdall JWKS.
type JWTAuth struct {
	validator *validator.Validator
	config    JWTAuthConfig
}

// ParsePrincipal extracts the principal and email from the JWT claims.
// Email is present when Authelia's oidc_contextualizer resolves it; it is empty
// for M2M or anonymous tokens.
func (j *JWTAuth) ParsePrincipal(ctx context.Context, token string, logger *slog.Logger) (string, string, error) {
	// When JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL is set, return it instead of the JWT principal.
	if j.config.MockLocalPrincipal != "" {
		logger.DebugContext(ctx, "JWT authentication is disabled, using configured mock principal")
		return j.config.MockLocalPrincipal, j.config.MockLocalEmail, nil
	}

	if j.validator == nil {
		return "", "", errors.New("JWT validator is not set up")
	}

	parsedJWT, err := j.validator.ValidateToken(ctx, token)
	if err != nil {
		logger.With("audience", j.config.Audience).With("issuer", j.config.Issuer).With("error", err).WarnContext(ctx, "authorization failed")
		if errors.Is(err, domain.ErrAuthUnavailable) {
			// Key-provider failure (JWKS endpoint unreachable), not a bad
			// token: propagate the sentinel so the caller can map this to
			// a 5xx instead of a 401.
			return "", "", domain.ErrAuthUnavailable
		}
		errString := err.Error()
		firstColon := strings.Index(errString, ":")
		if firstColon != -1 && firstColon+1 < len(errString) {
			errString = strings.Replace(errString, ": go-jose/go-jose/jwt", "", 1)
			// Recompute firstColon against the post-Replace string: the
			// Replace can shorten errString, and indexing it with the
			// pre-Replace offset can run past the end.
			firstColon = strings.Index(errString, ":")
			if firstColon != -1 && firstColon+1 < len(errString) {
				secondColon := strings.Index(errString[firstColon+1:], ":")
				if secondColon != -1 {
					errString = errString[:firstColon+secondColon+1]
				}
			}
		}
		return "", "", errors.New(errString)
	}

	claims, ok := parsedJWT.(*validator.ValidatedClaims)
	if !ok {
		return "", "", errors.New("failed to get validated authorization claims")
	}

	customClaims, ok := claims.CustomClaims.(*HeimdallClaims)
	if !ok {
		return "", "", errors.New("failed to get custom authorization claims")
	}

	return customClaims.Principal, customClaims.Email, nil
}

// NewJWTAuth creates a new JWT authentication service
func NewJWTAuth(config JWTAuthConfig) (*JWTAuth, error) {
	// Set up defaults if not provided
	jwksURLStr := config.JWKSURL
	if jwksURLStr == "" {
		jwksURLStr = defaultJWKSURL
	}
	audience := config.Audience
	if audience == "" {
		audience = defaultAudience
	}
	issuerStr := config.Issuer
	if issuerStr == "" {
		issuerStr = defaultIssuer
	}

	// Set up Heimdall JWKS key provider.
	jwksURL, err := url.Parse(jwksURLStr)
	if err != nil {
		slog.With("error", err).Error("invalid JWKS_URL")
		return nil, err
	}
	var issuer *url.URL
	issuer, err = url.Parse(issuerStr)
	if err != nil {
		slog.With("error", err).Error("invalid ISSUER")
		return nil, err
	}
	otelClient := &http.Client{
		Transport: otelhttp.NewTransport(http.DefaultTransport),
		Timeout:   jwksClientTimeout,
	}
	provider := jwks.NewCachingProvider(issuer, 5*time.Minute, jwks.WithCustomJWKSURI(jwksURL), jwks.WithCustomClient(otelClient))

	// Wrap the provider's KeyFunc so a JWKS fetch failure (endpoint down,
	// network error) is distinguishable from an actual bad token: the
	// validator library wraps whatever this returns with %w on every layer
	// up to ValidateToken's result, so errors.Is(err, domain.ErrAuthUnavailable)
	// still matches after that wrapping.
	keyFunc := func(ctx context.Context) (interface{}, error) {
		key, err := provider.KeyFunc(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", domain.ErrAuthUnavailable, err)
		}
		return key, nil
	}

	// Set up the JWT validator.
	jwtValidator, err := validator.New(
		keyFunc,
		signatureAlgorithm,
		issuer.String(),
		[]string{audience},
		validator.WithCustomClaims(customClaims),
		validator.WithAllowedClockSkew(5*time.Second),
	)
	if err != nil {
		slog.With("error", err).Error("failed to set up the Heimdall JWT validator")
		return nil, err
	}

	config.Audience = audience
	config.Issuer = issuerStr

	return &JWTAuth{
		validator: jwtValidator,
		config:    config,
	}, nil
}
