// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"

	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
)

// validateAssignee refuses assigning to someone who holds no direct grant
// (writer or auditor) on the project. Assignment itself is data only and
// grants nothing on its own — this only catches assigning to someone with
// no standing on the project at all, e.g. a typo'd username.
//
// A nil ProjectReader skips the check rather than refusing every assignment,
// the same conservative handling GetFormation already gives this same missing
// dependency for the announcement date. This does mean the check is
// currently a no-op in the deployed system — assignee_not_on_project is
// advertised in the wire contract but cannot fire yet.
//
// What is missing is not the transport, which now exists, but a project-service
// subject returning the settings record: only writers have a lookup, and there
// is no auditors equivalent. Answering with writers alone would make this
// refuse every auditor, so the reader stays nil until both halves are
// readable. See ProjectReaderImpl.
//
// UpdateItem calls this before opening its transaction, so this round-trip does
// not hold a row lock for its duration, and carries the result into the
// transaction to report from the position the check used to occupy. The rationale
// for both halves is at that call site.
func validateAssignee(ctx context.Context, projects port.ProjectReader, projectUID, assignee string) error {
	if projects == nil {
		return nil
	}

	settings, err := projects.GetSettings(ctx, projectUID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return domain.NewReasonError(domain.ErrInvalidRequest, reasonAssigneeNotOnProject)
		}
		return err
	}

	for _, w := range settings.Writers {
		if w == assignee {
			return nil
		}
	}
	for _, a := range settings.Auditors {
		if a == assignee {
			return nil
		}
	}
	return domain.NewReasonError(domain.ErrInvalidRequest, reasonAssigneeNotOnProject)
}
