// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/infrastructure/mock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepairApplicationsRepublishesLiveRows(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)

	report, err := d.service.RepairApplications(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, report.LiveAttempted)
	assert.Zero(t, report.DeletedAttempted)
	require.Len(t, d.access.Published(), 2)
	require.Len(t, d.indexer.ApplicationPublished(), 2)
	assert.Equal(t, created.UID, d.indexer.ApplicationPublished()[1].ApplicationUID)
}

func TestRepairApplicationsDoesNotRequireAUnitOfWork(t *testing.T) {
	d := applicationService(t)
	submitOne(t, d.service)
	repairer := NewService(
		WithApplications(d.applications),
		WithApplicationAccess(d.access),
		WithApplicationIndexer(d.indexer),
		WithApplicationTeam("formation"),
	)

	report, err := repairer.RepairApplications(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, report.LiveAttempted)
	assert.Zero(t, report.Failed)
}

func TestRepairApplicationsRetriesRetainedDeletionMarkers(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)
	require.NoError(t, d.service.DeleteApplication(
		asPrincipal("asmith"),
		&svc.DeleteApplicationPayload{
			Version: "1", UID: created.UID, IfMatch: created.Revision,
		},
	))

	report, err := d.service.RepairApplications(context.Background())

	require.NoError(t, err)
	assert.Zero(t, report.LiveAttempted)
	assert.Equal(t, 1, report.DeletedAttempted)
	assert.Zero(t, report.Failed)
	assert.Equal(t, []string{created.UID, created.UID}, d.access.Deleted())
	assert.Equal(t, []string{created.UID, created.UID}, d.indexer.ApplicationDeleted())
}

func TestRepairApplicationsVisitsEveryPage(t *testing.T) {
	d := applicationService(t)
	total := applicationRepairPageSize + 1
	for i := range total {
		_, err := d.applications.Create(context.Background(), &model.Application{
			SubmitterUsername: fmt.Sprintf("asmith%d", i),
			Payload:           map[string]any{"project_name": "Proposed Project"},
		})
		require.NoError(t, err)
	}

	report, err := d.service.RepairApplications(context.Background())

	require.NoError(t, err)
	assert.Equal(t, total, report.LiveAttempted)
	assert.Zero(t, report.Failed)
	assert.Len(t, d.indexer.ApplicationPublished(), total)
}

// One failing row must not stop the rest of the page.
func TestRepairApplicationsCountsOneFailureAndContinues(t *testing.T) {
	d := applicationService(t)
	failing := submitOne(t, d.service)
	healthy := submitOne(t, d.service)
	repairer := NewService(
		WithApplications(d.applications),
		WithApplicationAccess(d.access),
		WithApplicationIndexer(&failOneApplication{
			IndexerPublisher: d.indexer, uid: failing.UID,
		}),
		WithApplicationTeam("formation"),
	)

	report, err := repairer.RepairApplications(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 2, report.LiveAttempted)
	assert.Equal(t, 1, report.Failed)
	require.NotNil(t, d.indexer.LatestApplication(healthy.UID))
}

func TestRepairApplicationsSkipsRowDeletedAfterListing(t *testing.T) {
	d := applicationService(t)
	created := submitOne(t, d.service)
	repository := &deleteAfterListApplicationRepository{
		ApplicationRepository: d.applications,
		uid:                   mustUUID(t, created.UID),
		revision:              created.Revision,
	}
	repairer := NewService(
		WithApplications(repository),
		WithApplicationAccess(d.access),
		WithApplicationIndexer(d.indexer),
		WithApplicationTeam("formation"),
	)

	report, err := repairer.RepairApplications(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, report.LiveAttempted)
	assert.Equal(t, 1, report.DeletedAttempted)
	assert.Zero(t, report.Failed)
	assert.Len(t, d.indexer.ApplicationPublished(), 1)
	assert.Equal(t, []string{created.UID}, d.access.Deleted())
	assert.Equal(t, []string{created.UID}, d.indexer.ApplicationDeleted())
}

type deleteAfterListApplicationRepository struct {
	port.ApplicationRepository
	uid      uuid.UUID
	revision int64
}

func (r *deleteAfterListApplicationRepository) ListRepairPage(
	ctx context.Context, after uuid.UUID, limit int,
) ([]*model.Application, error) {
	page, err := r.ApplicationRepository.ListRepairPage(ctx, after, limit)
	if err != nil || len(page) == 0 {
		return page, err
	}
	_, err = r.Delete(ctx, r.uid, r.revision)
	return page, err
}

// failOneApplication fails the index publish for a single application.
type failOneApplication struct {
	*mock.IndexerPublisher
	uid string
}

func (f *failOneApplication) PublishApplication(
	ctx context.Context, doc *port.ApplicationProjection,
) error {
	if doc.ApplicationUID == f.uid {
		return assert.AnError
	}
	return f.IndexerPublisher.PublishApplication(ctx, doc)
}

func TestReconcileRepairsApplicationsWithoutAProjectReader(t *testing.T) {
	d := applicationService(t)
	submitOne(t, d.service)
	reconciler := NewReconciler(nil, nil, nil, nil, nil, nil, time.Minute)
	reconciler.SetApplicationRepairer(d.service)

	report, err := reconciler.ReconcileOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 1, report.ApplicationLiveAttempted)
	require.Len(t, d.indexer.ApplicationPublished(), 2)
}
