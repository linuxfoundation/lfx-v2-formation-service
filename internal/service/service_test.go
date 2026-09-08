// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAuthenticator is a port.Authenticator double that returns whatever
// this test case configured.
type fakeAuthenticator struct {
	principal string
	email     string
	err       error
}

func (f fakeAuthenticator) ParsePrincipal(context.Context, string, *slog.Logger) (string, string, error) {
	return f.principal, f.email, f.err
}

// fakePinger lets tests drive readiness without a real database.
type fakePinger struct {
	err error
}

func (f fakePinger) Ping(context.Context) error { return f.err }

func TestServiceReady(t *testing.T) {
	tests := []struct {
		name    string
		service *Service
		want    bool
	}{
		{
			name:    "ready with no database wired",
			service: NewService(),
			want:    true,
		},
		{
			name:    "ready when the database pings successfully",
			service: NewService(WithDB(fakePinger{})),
			want:    true,
		},
		{
			name:    "not ready when the database ping fails",
			service: NewService(WithDB(fakePinger{err: errors.New("connection refused")})),
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.service.ServiceReady(context.Background()))
		})
	}
}

func TestJWTAuth(t *testing.T) {
	t.Run("no authenticator wired falls through as a plain error", func(t *testing.T) {
		s := NewService()

		_, err := s.JWTAuth(context.Background(), "token", nil)

		require := assert.New(t)
		require.Error(err)
		var unauthorized *svc.UnauthorizedError
		require.NotErrorAs(err, &unauthorized)
	})

	t.Run("valid token sets the principal and email on the context", func(t *testing.T) {
		s := NewService(WithAuth(fakeAuthenticator{principal: "user-1", email: "user@example.com"}))

		ctx, err := s.JWTAuth(context.Background(), "token", nil)

		assert.NoError(t, err)
		require.NotNil(t, ctx)
		// Asserting the claim values, not just a non-nil context: returning
		// the input context untouched would satisfy the latter.
		assert.Equal(t, "user-1", ctx.Value(constants.PrincipalContextID))
		assert.Equal(t, "user@example.com", ctx.Value(constants.EmailContextID))
	})

	t.Run("bad token maps to the declared UnauthorizedError", func(t *testing.T) {
		s := NewService(WithAuth(fakeAuthenticator{err: errors.New("token is expired")}))

		_, err := s.JWTAuth(context.Background(), "token", nil)

		var unauthorized *svc.UnauthorizedError
		assert.ErrorAs(t, err, &unauthorized)
	})

	t.Run("key provider failure falls through as a plain error, not 401", func(t *testing.T) {
		s := NewService(WithAuth(fakeAuthenticator{err: domain.ErrAuthUnavailable}))

		_, err := s.JWTAuth(context.Background(), "token", nil)

		require := assert.New(t)
		require.Error(err)
		var unauthorized *svc.UnauthorizedError
		require.NotErrorAs(err, &unauthorized)
	})
}

func TestLivez(t *testing.T) {
	s := NewService()

	result, err := s.Livez(context.Background())

	assert.NoError(t, err)
	assert.Equal(t, "OK\n", string(result))
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name         string
		service      *Service
		expectError  bool
		expectedBody string
	}{
		{
			name:         "ready returns OK",
			service:      NewService(),
			expectError:  false,
			expectedBody: "OK\n",
		},
		{
			name:        "database unavailable returns ServiceUnavailable",
			service:     NewService(WithDB(fakePinger{err: errors.New("connection refused")})),
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.service.Readyz(context.Background())

			if tt.expectError {
				assert.Error(t, err)
				var unavailable *svc.ServiceUnavailableError
				assert.ErrorAs(t, err, &unavailable)
				assert.Nil(t, result)
				return
			}

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedBody, string(result))
		})
	}
}
