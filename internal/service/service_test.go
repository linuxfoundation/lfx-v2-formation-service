// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/stretchr/testify/assert"
)

func TestServiceReady(t *testing.T) {
	tests := []struct {
		name    string
		service *Service
		want    bool
	}{
		{
			name:    "ready when constructed",
			service: NewService(),
			want:    true,
		},
		{
			name:    "not ready when flag false",
			service: &Service{ready: false},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.service.ServiceReady())
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
			name:        "not ready returns ServiceUnavailable",
			service:     &Service{ready: false},
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
