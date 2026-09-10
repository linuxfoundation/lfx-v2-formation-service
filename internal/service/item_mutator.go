// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service/email"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// The machine-readable reasons this route can refuse with. They exist so the
// browser can switch on the cause rather than parse a message, which is why
// several distinct ones share a single HTTP status.
// self_acceptance_forbidden belongs to the accept route, not this one.
const (
	reasonNotFound             = "not_found"
	reasonVersionMismatch      = "version_mismatch"
	reasonUnknownItemKey       = "unknown_item_key"
	reasonChecklistReadOnly    = "checklist_read_only"
	reasonInvalidTransition    = "invalid_transition"
	reasonSkipReasonRequired   = "skip_reason_required"
	reasonAssigneeNotOnProject = "assignee_not_on_project"
	reasonLinkSchemeInvalid    = "link_scheme_invalid"
	reasonDueDateInvalid       = "due_date_invalid"
	reasonSubItemNull          = "sub_item_null"
	reasonUnknownSubItemKey    = "unknown_sub_item_key"
	reasonNoFieldsToUpdate     = "no_fields_to_update"
)

// dueDateLayout is the wire format for due_date: YYYY-MM-DD, matching the
// read side's dsl.FormatDate. The write side can't use that same Goa format
// validator (see design.go's due_date attribute comment), so this format is
// enforced here instead.
const dueDateLayout = "2006-01-02"

// reasonMessages gives each reason a human-readable message, independent of
// the generic sentinel error text. The UI is told to switch on reason, not
// on this string, but a caller reading the response directly still needs a
// sentence, not "conflict".
var reasonMessages = map[string]string{
	reasonNotFound:             "no formation exists for this project",
	reasonUnknownItemKey:       "no item with that key exists",
	reasonVersionMismatch:      "if-match did not match the item's current version",
	reasonChecklistReadOnly:    "the checklist is completed or frozen and no longer accepts changes",
	reasonInvalidTransition:    "that status transition is not permitted from the item's current state",
	reasonSkipReasonRequired:   "a reason is required to skip an item",
	reasonAssigneeNotOnProject: "the assignee holds no writer or auditor grant on this project",
	reasonLinkSchemeInvalid:    "evidence_link must use the http or https scheme",
	reasonDueDateInvalid:       "due_date must be YYYY-MM-DD, or an empty string to clear it",
	reasonSubItemNull:          "sub_items must not contain null entries",
	reasonUnknownSubItemKey:    "the item has no sub-item with that key",
	reasonNoFieldsToUpdate:     "the request changes no field",
}

// allowedItemTransitions is every status edge this route may make. done is
// deliberately unreachable from any source here, not just excluded as a
// source: this route's guard is Manage (any project writer, including the
// item's own assignee), and reaching done has to go through
// awaiting_acceptance and the accept route so the formation-team-only,
// never-the-assignee guard on acceptance actually runs. A PATCH straight to
// done would let a writer accept their own item by skipping that route
// entirely, which is exactly what self_acceptance_forbidden exists to
// prevent.
var allowedItemTransitions = map[model.ItemStatus][]model.ItemStatus{
	model.StatusNotStarted: {model.StatusInProgress, model.StatusSkipped},
	model.StatusInProgress: {model.StatusBlocked, model.StatusAwaitingAcceptance},
	model.StatusBlocked:    {model.StatusInProgress},
	model.StatusSkipped:    {model.StatusNotStarted},
}

func isAllowedItemTransition(from, to model.ItemStatus) bool {
	for _, allowed := range allowedItemTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// UpdateItem changes one checklist item: status, note, due date, skip
// reason, evidence link, assignee, or sub-items. The whole read-validate-
// write-log sequence runs inside one transaction, so a failure partway —
// most concretely, the activity append after the item write succeeds —
// leaves neither change committed rather than an item mutation with no
// audit trail.
func (s *Service) UpdateItem(ctx context.Context, p *svc.UpdateItemPayload) (*svc.FormationItem, error) {
	if s.uow == nil {
		// Server misconfiguration, not a client problem: falls through
		// Goa's default formatter as a 500, same rationale as the
		// nil-authenticator branch in JWTAuth.
		return nil, errors.New("no unit of work wired")
	}

	// Checked before the transaction opens, because it is a NATS round-trip to
	// another service bounded only by that client's timeout, and inside the
	// transaction it would hold the row lock the later Update takes for the
	// whole trip — the same reason the template upgrade resolves its project
	// facts before opening one.
	//
	// The result is carried, not returned, and reported by buildItemPatch from
	// the exact position the check used to occupy. Returning it here would put
	// this refusal ahead of the missing-checklist, read-only, stale-precondition
	// and invalid-transition ones, so a stale write to a frozen checklist would
	// answer 400 where it used to answer 409 — a change to the wire contract,
	// made by accident, in the name of not holding a lock.
	//
	// The cost is one wasted round-trip when the request was going to be
	// refused on a precondition anyway. That is the right trade against holding
	// a row lock across a call to another service.
	var assigneeErr error
	if p.Assignee != nil && *p.Assignee != "" {
		assigneeErr = validateAssignee(ctx, s.projects, p.ProjectUID, *p.Assignee)
	}

	var result *model.Item
	txErr := s.uow.Do(ctx, func(tx port.Tx) error {
		formation, err := tx.Formations().GetByProject(ctx, p.ProjectUID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonError(domain.ErrNotFound, reasonNotFound)
			}
			return err
		}

		// A stale precondition and a read-only checklist are both
		// refused before touching the item at all, so a caller cannot
		// tell the two apart from timing.
		if !formation.Lifecycle.Mutable() {
			return domain.NewReasonError(domain.ErrConflict, reasonChecklistReadOnly)
		}

		item, err := tx.Items().GetByKey(ctx, formation.UID, p.ItemKey)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonErrorf(domain.ErrNotFound, reasonUnknownItemKey,
					"no item with key %q exists", p.ItemKey)
			}
			return err
		}
		if item.Revision != p.IfMatch {
			return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
		}

		patch, err := buildItemPatch(item, p, assigneeErr)
		if err != nil {
			return err
		}

		updated, err := tx.Items().Update(ctx, item.UID, item.Revision, patch)
		if err != nil {
			if errors.Is(err, domain.ErrVersionMismatch) {
				return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
			}
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonError(domain.ErrNotFound, reasonUnknownItemKey)
			}
			return err
		}

		principal, _ := ctx.Value(constants.PrincipalContextID).(string)
		if err := tx.Activity().Append(ctx, &model.ActivityEntry{
			FormationUID: formation.UID,
			ItemUID:      &updated.UID,
			Actor:        principal,
			SetBy:        model.SetByUser,
			Action:       mutationAction(p),
			Before:       activitySummary(item),
			After:        activitySummary(updated),
		}); err != nil {
			return err
		}

		result = updated
		return nil
	})
	if txErr != nil {
		return nil, mapItemMutationError(txErr)
	}

	// Fire the item-assigned email when the assignee was set (or changed) by
	// this request. Dispatch is best-effort: a failure is logged and never
	// blocks the write that already succeeded. The email service itself uses
	// NATS core with no redelivery, so there is no retry contract to honour.
	if p.Assignee != nil && *p.Assignee != "" {
		s.dispatchItemAssigned(ctx, p.ProjectUID, result)
	}

	return itemToWire(result), nil
}

// dispatchItemAssigned sends the item-assigned notification email.
// It is a no-op when the emailer is not wired or email is disabled.
func (s *Service) dispatchItemAssigned(ctx context.Context, projectUID string, item *model.Item) {
	if s.emailer == nil || !s.emailCfg.Enabled {
		return
	}

	// The assignee is stored as a username. Resolve it to an email address
	// using the project settings roster; the email field on each grantee
	// entry is what the project service carries alongside the username.
	// A missing or unresolvable address is logged and silently skipped —
	// the write already succeeded and best-effort dispatch must not block it.
	var to string
	if s.projects != nil {
		settings, err := s.projects.GetSettings(ctx, projectUID)
		if err != nil {
			slog.WarnContext(ctx, "item-assigned email: could not read project settings; not sent",
				"item_key", item.ItemKey, "assignee", item.Assignee, "error", err)
			return
		}
		to = settings.UserEmails[item.Assignee]
	}
	if to == "" {
		slog.WarnContext(ctx, "item-assigned email: no email address on record for assignee; not sent",
			"item_key", item.ItemKey, "assignee", item.Assignee)
		return
	}

	projectName, slug := s.projectNameAndSlug(ctx, projectUID)
	checklistURL := fmt.Sprintf("%s/manage/projects/%s/checklist", s.emailCfg.AdminBaseURL, slug)
	if slug == "" {
		// Fall back to a URL keyed on the project UID when the slug is unavailable.
		checklistURL = fmt.Sprintf("%s/manage/projects/%s/checklist", s.emailCfg.AdminBaseURL, projectUID)
	}

	var dueDate string
	if item.DueDate != nil {
		dueDate = item.DueDate.Format("2006-01-02")
	}

	subject, html, text, err := email.RenderItemAssigned(email.ItemAssignedData{
		ProjectName:  projectName,
		ItemTitle:    item.Title,
		IsGating:     item.Gate,
		DueDate:      dueDate,
		ChecklistURL: checklistURL,
	})
	if err != nil {
		slog.WarnContext(ctx, "item-assigned email: render failed; not sent",
			"item_key", item.ItemKey, "assignee", item.Assignee, "error", err)
		return
	}

	if sendErr := s.emailer.Send(ctx, port.EmailMessage{
		To:      to,
		Subject: subject,
		HTML:    html,
		Text:    text,
		GroupID: "formation.item_assigned",
	}); sendErr != nil {
		slog.WarnContext(ctx, "item-assigned email: send failed",
			"item_key", item.ItemKey, "assignee", item.Assignee, "to", to, "error", sendErr)
	}
}

// projectNameAndSlug returns the project's display name and slug. Both
// degrade to empty string when the project reader is not wired or the
// lookup fails, so an email can still be sent with a UID-based URL rather
// than failing the whole dispatch.
func (s *Service) projectNameAndSlug(ctx context.Context, projectUID string) (name, slug string) {
	if s.projects == nil {
		return "", ""
	}
	var err error
	name, err = s.projects.Name(ctx, projectUID)
	if err != nil {
		slog.WarnContext(ctx, "email: could not resolve project name", "project_uid", projectUID, "error", err)
	}
	slug, err = s.projects.Slug(ctx, projectUID)
	if err != nil {
		slog.WarnContext(ctx, "email: could not resolve project slug", "project_uid", projectUID, "error", err)
	}
	return name, slug
}

// buildItemPatch validates the payload's mutable fields against the item's
// current state and turns them into a port.ItemPatch. It returns before
// setting anything on the returned patch once it finds a refusal, so a
// caller never sees a partial patch alongside an error.
// assigneeErr carries the result of the assignee check the caller ran before
// opening the transaction, so it can be reported from the position it used to be
// checked in.
func buildItemPatch(item *model.Item, p *svc.UpdateItemPayload, assigneeErr error) (port.ItemPatch, error) {
	var patch port.ItemPatch

	// Refused rather than applied as a no-op: Update always increments the
	// revision, so an empty body would invalidate every other client's
	// if_match and append an activity entry recording no change.
	if p.Status == nil && p.Assignee == nil && p.Note == nil && p.SkipReason == nil &&
		p.EvidenceLink == nil && p.DueDate == nil && p.SubItems == nil {
		return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonNoFieldsToUpdate)
	}

	if p.Status != nil {
		newStatus := model.ItemStatus(*p.Status)
		if newStatus != item.Status && !isAllowedItemTransition(item.Status, newStatus) {
			return port.ItemPatch{}, domain.NewReasonError(domain.ErrConflict, reasonInvalidTransition)
		}
		patch.Status = &newStatus
	}

	// The skipped-item invariant is checked against the values this PATCH
	// resolves to, not against the fields it happens to carry. Scoping it to
	// requests that include status left two ways to reach a state the
	// skip_needs_reason constraint rejects: an already-skipped item can send
	// skip_reason alone, and the constraint compares btrim(skip_reason), so a
	// whitespace-only reason passes an == "" test here. Either one used to
	// surface as a 500 from Postgres while the mock stored it happily.
	resolvedStatus := item.Status
	if patch.Status != nil {
		resolvedStatus = *patch.Status
	}
	if resolvedStatus == model.StatusSkipped {
		resolvedReason := item.SkipReason
		if p.SkipReason != nil {
			resolvedReason = *p.SkipReason
		}
		if strings.TrimSpace(resolvedReason) == "" {
			return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonSkipReasonRequired)
		}
	}

	if p.Assignee != nil {
		// The membership itself was checked by the caller, before the
		// transaction opened, because it is a call to another service. It is
		// reported here, where the check used to happen, so which refusal a
		// caller sees does not depend on where the check runs: an invalid
		// transition and a skipped item with no reason both still outrank it.
		if assigneeErr != nil {
			return port.ItemPatch{}, assigneeErr
		}
		patch.Assignee = p.Assignee
	}

	if p.EvidenceLink != nil {
		if *p.EvidenceLink != "" && !isSafeURL(*p.EvidenceLink) {
			return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonLinkSchemeInvalid)
		}
		patch.EvidenceLink = p.EvidenceLink
	}

	if p.DueDate != nil {
		if *p.DueDate != "" {
			due, err := time.Parse(dueDateLayout, *p.DueDate)
			// Go's parser accepts a year zero and Postgres has none, so
			// without this the value reached the DATE column and came back as
			// a 500 rather than this 400.
			if err != nil || due.Year() < 1 {
				return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonDueDateInvalid)
			}
		}
		patch.DueDate = p.DueDate
	}
	if p.Note != nil {
		patch.Note = p.Note
	}
	if p.SkipReason != nil {
		patch.SkipReason = p.SkipReason
	}
	if p.SubItems != nil {
		// Refused rather than skipped. Goa's generated validator steps over
		// nil elements without reporting them, so `"sub_items":[null]`
		// arrives here as a live nil and used to panic the handler on the
		// first field access. Answering 400 also tells the caller their
		// payload was wrong, which quietly dropping the entry would not.
		for _, u := range p.SubItems {
			if u == nil {
				return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonSubItemNull)
			}
		}
		subItems, err := subItemsFromWire(item.SubItems, p.SubItems)
		if err != nil {
			return port.ItemPatch{}, err
		}
		patch.SubItems = &subItems
	}

	return patch, nil
}

// subItemsFromWire merges updates into existing by key. The payload's rule that
// a caller sends only the fields being changed applies within sub_items too, so
// a caller naming one sub-item must not silently drop
// every other one from the array a plain replace would have produced. Each
// update also carries its title forward from existing, since
// FormationSubItemUpdate (the write shape) has no title field — sub-items are
// copied from the template and immutable except for status. A key the item
// does not have is therefore refused by the caller rather than appended:
// appending built a row with no title, which is both a malformed display row
// and a way to change checklist structure through a status-only route.
func subItemsFromWire(existing []model.SubItem, updates []*svc.FormationSubItemUpdate) ([]model.SubItem, error) {
	out := make([]model.SubItem, len(existing))
	copy(out, existing)

	index := make(map[string]int, len(out))
	for i, si := range out {
		index[si.Key] = i
	}

	for _, u := range updates {
		if u == nil {
			// buildItemPatch rejects these before we get here; this keeps a
			// future caller from reintroducing the panic.
			continue
		}
		i, ok := index[u.Key]
		if !ok {
			return nil, domain.NewReasonErrorf(domain.ErrInvalidRequest, reasonUnknownSubItemKey,
				"the item has no sub-item with key %q", u.Key)
		}
		out[i].Status = model.ItemStatus(u.Status)
	}
	return out, nil
}

// mutationAction names the activity entry after the field the caller led
// with. A PATCH can touch several fields at once, but the feed gets exactly
// one entry per call, so this picks one representative action rather than
// logging every field separately.
func mutationAction(p *svc.UpdateItemPayload) string {
	switch {
	case p.Status != nil:
		return "status_changed"
	case p.Assignee != nil:
		return "assignee_changed"
	case p.EvidenceLink != nil:
		return "evidence_link_changed"
	case p.DueDate != nil:
		return "due_date_changed"
	case p.Note != nil:
		return "note_changed"
	case p.SubItems != nil:
		return "sub_items_changed"
	// Last so that adding it left every other combination's label alone; a
	// skip reason only ever travels with a status in practice, and status
	// already wins.
	case p.SkipReason != nil:
		return "skip_reason_changed"
	default:
		return "item_updated"
	}
}

// activitySummary is a redacted before/after snapshot: the status and
// assignee only, not the whole row (data-model.md, "Before and After hold
// redacted summaries, not whole rows").
func activitySummary(item *model.Item) map[string]any {
	return map[string]any{
		"status":   string(item.Status),
		"assignee": item.Assignee,
	}
}

// mapItemMutationError turns a domain.ReasonError into the declared
// FormationError the design requires, setting Name to the Error() the
// design declared it under so Goa's generated encoder picks the right HTTP
// status. An error that is not a ReasonError is returned unchanged and
// falls through as a 500 — every expected refusal from this method is a
// ReasonError, so anything else is unexpected.
func mapItemMutationError(err error) error {
	return mapReasonError(err, reasonMessages)
}

// mapReasonError is the single translation from a domain refusal to a declared
// FormationError, shared by every route that returns one.
//
// The status switch lives here once because it is the same switch everywhere:
// the design declares one error per domain sentinel, and a route that mapped a
// conflict to a different status than its neighbour would be a bug rather than a
// variation. Vocabulary is what differs between routes, so each passes its own
// message maps, consulted in order.
func mapReasonError(err error, messages ...map[string]string) error {
	var re *domain.ReasonError
	if !errors.As(err, &re) {
		return err
	}

	message := re.Message
	for _, m := range messages {
		if message != "" {
			break
		}
		message = m[re.Reason]
	}
	if message == "" {
		message = re.Err.Error()
	}

	var name, code string
	switch {
	case errors.Is(re.Err, domain.ErrNotFound):
		name, code = "NotFound", "404"
	case errors.Is(re.Err, domain.ErrVersionMismatch):
		name, code = "VersionMismatch", "412"
	case errors.Is(re.Err, domain.ErrConflict):
		name, code = "Conflict", "409"
	case errors.Is(re.Err, domain.ErrInvalidRequest):
		name, code = "BadRequest", "400"
	default:
		return err
	}

	return &svc.FormationError{Name: name, Code: code, Message: message, Reason: re.Reason}
}
