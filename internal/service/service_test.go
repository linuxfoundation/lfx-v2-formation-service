// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"testing"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/stretchr/testify/assert"
)

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
