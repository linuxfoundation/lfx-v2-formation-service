// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
)

const applicationRepairPageSize = 100

// ApplicationRepairReport summarizes one repair sweep.
type ApplicationRepairReport struct {
	LiveAttempted    int
	DeletedAttempted int
	Failed           int
}

// RepairApplications republishes every live application and every retained
// deletion marker. Repeated runs are safe.
func (s *Service) RepairApplications(
	ctx context.Context,
) (*ApplicationRepairReport, error) {
	if s.applications == nil {
		return nil, errors.New("application repair storage is not available")
	}
	report := &ApplicationRepairReport{}
	deletedErr := s.repairDeletedApplications(ctx, report)
	liveErr := s.repairLiveApplications(ctx, report)
	return report, errors.Join(liveErr, deletedErr)
}

func (s *Service) repairLiveApplications(
	ctx context.Context, report *ApplicationRepairReport,
) error {
	var after uuid.UUID
	for {
		page, err := s.applications.ListRepairPage(ctx, after, applicationRepairPageSize)
		if err != nil {
			return fmt.Errorf("list live applications for repair: %w", err)
		}
		for _, listed := range page {
			report.LiveAttempted++
			current, err := s.applications.Get(ctx, listed.UID)
			if errors.Is(err, domain.ErrNotFound) {
				continue
			}
			if err == nil {
				err = s.publishApplication(ctx, current)
			}
			if err != nil {
				report.Failed++
			}
		}
		if len(page) < applicationRepairPageSize {
			return nil
		}
		after = page[len(page)-1].UID
	}
}

func (s *Service) repairDeletedApplications(
	ctx context.Context, report *ApplicationRepairReport,
) error {
	var after uuid.UUID
	for {
		page, err := s.applications.ListDeletionPage(ctx, after, applicationRepairPageSize)
		if err != nil {
			return fmt.Errorf("list application deletions for repair: %w", err)
		}
		for _, marker := range page {
			report.DeletedAttempted++
			if err := s.deleteApplicationProjection(ctx, marker.UID); err != nil {
				report.Failed++
			}
		}
		if len(page) < applicationRepairPageSize {
			return nil
		}
		after = page[len(page)-1].UID
	}
}
