// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// These helpers previously lived in acceptance_test.go, alongside the three
// routes that have since been removed. They are used across the package, so
// they moved here rather than going with it.

// asPrincipal returns a context carrying a caller identity, which the write
// route requires: an activity entry naming nobody evidences nothing.
func asPrincipal(username string) context.Context {
	return context.WithValue(context.Background(), constants.PrincipalContextID, username)
}

func updateItem(s *Service, ctx context.Context, p *svc.UpdateItemPayload) (*svc.FormationItem, error) {
	res, err := s.UpdateItem(ctx, p)
	if err != nil {
		return nil, err
	}
	return res.Item, nil
}

// ptr is for the optional payload fields, which are pointers so that "absent"
// and "set to empty" stay distinguishable on the wire.
func ptr[T any](v T) *T { return &v }

func assignItem(s *Service, ctx context.Context, p *svc.AssignItemPayload) (*svc.FormationItem, error) {
	res, err := s.AssignItem(ctx, p)
	if err != nil {
		return nil, err
	}
	return res.Item, nil
}

func setStatus(s *Service, ctx context.Context, p *svc.SetItemStatusPayload) (*svc.FormationItem, error) {
	res, err := s.SetItemStatus(ctx, p)
	if err != nil {
		return nil, err
	}
	return res.Item, nil
}

func formationError(t *testing.T, err error) *svc.FormationError {
	t.Helper()
	require.Error(t, err)
	fe, ok := err.(*svc.FormationError)
	require.True(t, ok, "error is %T, want *svc.FormationError: %v", err, err)
	return fe
}
