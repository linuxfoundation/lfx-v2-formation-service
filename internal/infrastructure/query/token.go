// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package query

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/auth0/go-auth0/authentication"
	"github.com/auth0/go-auth0/authentication/oauth"
	"golang.org/x/oauth2"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// tokenExpiryLeeway shrinks the refresh window so a token that is about to
// expire is never presented in flight.
const tokenExpiryLeeway = 60 * time.Second

// IdentityConfig is the service identity used for outbound reads.
//
// The flow is an OAuth2 client-credentials grant with a private-key JWT
// client assertion (RS256) — there is no client secret to leak, and the same
// shape lfx-v2-committee-service uses to read the same layer. The principal
// the read layer filters on is the client id, so this identity is what the
// grant is held against.
type IdentityConfig struct {
	ClientID string

	// PrivateKey is an RSA private key in PEM form, used to sign the
	// assertion.
	PrivateKey string

	// Domain is the Auth0 tenant, e.g. linuxfoundation-dev.auth0.com.
	Domain string

	// Audience is optional.
	Audience string
}

// auth0TokenSource is an oauth2.TokenSource over the Auth0 authentication
// client. Wrapped in oauth2.ReuseTokenSource by NewIdentityClient so a token
// is fetched once and refreshed on expiry rather than per request — a sweep
// makes one call per platform row per project, and minting a token for each
// would be a request to the IdP per checklist row.
type auth0TokenSource struct {
	ctx        context.Context
	authConfig *authentication.Authentication
	audience   string
}

// Token issues a client-credentials request, signing the assertion with the
// configured key.
func (a *auth0TokenSource) Token() (*oauth2.Token, error) {
	ctx := a.ctx
	if ctx == nil {
		ctx = context.TODO()
	}
	tokenSet, err := a.authConfig.OAuth.LoginWithClientCredentials(ctx,
		oauth.LoginWithClientCredentialsRequest{Audience: a.audience},
		oauth.IDTokenValidationOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("obtain M2M token: %w", err)
	}
	return &oauth2.Token{
		AccessToken: tokenSet.AccessToken,
		TokenType:   tokenSet.TokenType,
		Expiry:      time.Now().Add(time.Duration(tokenSet.ExpiresIn)*time.Second - tokenExpiryLeeway),
	}, nil
}

// NewIdentityClient builds the *http.Client the query Client reads through.
// It attaches the service identity's bearer token to every request and
// refreshes it as needed.
//
// It returns an error rather than degrading when the credentials are
// incomplete. An unauthenticated read of a per-principal filtered layer
// returns nothing readable, which would report every project as having done
// no work — the one failure mode the error taxonomy exists to prevent. The
// caller's choice is to configure the identity or to leave the read layer
// unconfigured entirely; a half-configured one is not a third option.
func NewIdentityClient(ctx context.Context, cfg IdentityConfig, timeout time.Duration) (*http.Client, error) {
	if cfg.ClientID == "" || cfg.PrivateKey == "" || cfg.Domain == "" {
		return nil, errors.New("incomplete service identity: client id, private key and domain are all required")
	}

	// The client the identity provider is reached through, supplied rather
	// than defaulted for two reasons. It bounds the calls made to the IdP:
	// constructing this fetches the tenant's JWKS, and minting a token later
	// retries on throttling, neither of which carries a deadline of its own —
	// and an unbounded mint stalls the sweep behind it, which then reaches no
	// project after the one it stalled on. It also keeps those retry and
	// client-info transports off http.DefaultClient, which is what the SDK
	// wraps when given no client, and which the rest of this process shares.
	authConfig, err := authentication.New(ctx,
		cfg.Domain,
		authentication.WithClientID(cfg.ClientID),
		authentication.WithClientAssertion(cfg.PrivateKey, "RS256"),
		authentication.WithClient(&http.Client{Timeout: timeout}),
	)
	if err != nil {
		// Reaching the tenant's metadata, almost always. The key is not read
		// here — it is parsed when the first token is minted — so a key that
		// is not a valid RSA PEM surfaces on the first sweep rather than now.
		return nil, fmt.Errorf("initialise M2M client for %s: %w", cfg.Domain, err)
	}

	tokenSource := oauth2.ReuseTokenSource(nil, &auth0TokenSource{
		ctx:        ctx,
		authConfig: authConfig,
		audience:   cfg.Audience,
	})

	// oauth2.NewClient builds the transport that attaches the token; the
	// context carries the base client underneath it, which is where the
	// tracing transport goes so outbound reads appear in a trace alongside
	// everything else this service does.
	base := &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport), Timeout: timeout}
	client := oauth2.NewClient(context.WithValue(ctx, oauth2.HTTPClient, base), tokenSource)
	client.Timeout = timeout
	return client, nil
}
