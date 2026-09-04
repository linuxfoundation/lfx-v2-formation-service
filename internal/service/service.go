// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package service provides the service implementations.
package service

import (
	"context"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
)

// Service implements the generated service interface.
//
// The service currently has no external runtime dependencies wired in. The
// readiness predicate is structured so that future dependency checks (e.g.,
// messaging, data stores) can be combined into ServiceReady without changing
// the health endpoint contract.
type Service struct {
	// ready reports whether the service can accept inbound requests. It is a
	// field (rather than a hardcoded return) so readiness can be exercised in
	// tests and so future dependency checks can be AND-ed in here.
	ready bool
}

// Ensure Service satisfies the generated service interface.
var _ svc.Service = (*Service)(nil)

// NewService constructs a Service that is ready to serve.
func NewService() *Service {
	return &Service{ready: true}
}

// ServiceReady reports whether the service is able to accept inbound requests.
//
// With no external dependencies wired, this returns true once the service is
// constructed. Future dependency checks should be combined here with logical
// AND (e.g., return s.ready && s.natsConn.IsConnected()).
func (s *Service) ServiceReady() bool {
	return s.ready
}

// Readyz implements the readiness probe.
func (s *Service) Readyz(_ context.Context) ([]byte, error) {
	if !s.ServiceReady() {
		return nil, &svc.ServiceUnavailableError{
			Code:    "503",
			Message: "The service is unavailable.",
		}
	}
	return []byte("OK\n"), nil
}

// Livez implements the liveness probe.
func (s *Service) Livez(_ context.Context) ([]byte, error) {
	// This always returns OK as long as the service is still running. As this
	// endpoint is used as a Kubernetes liveness check, the service must
	// self-detect non-recoverable errors and self-terminate.
	return []byte("OK\n"), nil
}
