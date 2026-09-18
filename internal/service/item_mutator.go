// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	svc "github.com/linuxfoundation/lfx-v2-formation-service/gen/lfx_v2_formation_service"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/model"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/domain/port"
	"github.com/linuxfoundation/lfx-v2-formation-service/internal/service/email"
	"github.com/linuxfoundation/lfx-v2-formation-service/pkg/constants"
)

// The machine-readable reasons the item write routes can refuse with. They
// exist so the browser can switch on the cause rather than parse a message,
// which is why several distinct ones share a single HTTP status. Shared across
// the three routes: the same condition must answer the same way whichever
// route met it.
const (
	reasonNotFound              = "not_found"
	reasonVersionMismatch       = "version_mismatch"
	reasonUnknownItemKey        = "unknown_item_key"
	reasonChecklistReadOnly     = "checklist_read_only"
	reasonInvalidTransition     = "invalid_transition"
	reasonBlockedReasonRequired = "blocked_reason_required"
	reasonSkipReasonRequired    = "skip_reason_required"
	reasonReturnReasonRequired  = "return_reason_required"
	reasonAssigneeNotOnProject  = "assignee_not_on_project"
	reasonLinkSchemeInvalid     = "link_scheme_invalid"
	reasonDueDateInvalid        = "due_date_invalid"
	reasonSubItemNull           = "sub_item_null"
	reasonUnknownSubItemKey     = "unknown_sub_item_key"
	reasonNoFieldsToUpdate      = "no_fields_to_update"
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
	reasonNotFound:              "no formation exists for this project",
	reasonUnknownItemKey:        "no item with that key exists",
	reasonVersionMismatch:       "if-match did not match the item's current version",
	reasonChecklistReadOnly:     "the checklist is completed or frozen and no longer accepts changes",
	reasonInvalidTransition:     "that status transition is not permitted from the item's current state",
	reasonBlockedReasonRequired: "a reason is required to block an item",
	reasonSkipReasonRequired:    "a reason is required to skip an item",
	reasonReturnReasonRequired:  "a reason is required to send an item back to not started",
	reasonAssigneeNotOnProject:  "the assignee holds no writer or auditor grant on this project",
	reasonLinkSchemeInvalid:     "evidence_link must use the http or https scheme",
	reasonDueDateInvalid:        "due_date must be YYYY-MM-DD, or an empty string to clear it",
	reasonSubItemNull:           "sub_items must not contain null entries",
	reasonUnknownSubItemKey:     "the item has no sub-item with that key",
	reasonNoFieldsToUpdate:      "the request changes no field",
}

// UpdateItem records an update against one checklist item: a note, an evidence
// link. The widest of the three item routes and the only one open on read
// access, because the person who did the work is often the one holding the
// least access — they say what they did here, and somebody else judges it.
//
// Nothing reachable from here moves a status or reassigns anybody. That is
// enforced by the payload rather than by a check: the fields do not exist on
// it, and their routes are guarded more narrowly.
func (s *Service) UpdateItem(ctx context.Context, p *svc.UpdateItemPayload) (*svc.UpdateItemResult, error) {
	if s.uow == nil {
		// Server misconfiguration, not a client problem: falls through
		// Goa's default formatter as a 500, same rationale as the
		// nil-authenticator branch in JWTAuth.
		return nil, errors.New("no unit of work wired")
	}

	result, _, txErr := s.mutateItem(ctx, p.ProjectUID, p.ItemKey, p.IfMatch,
		func(item *model.Item) (port.ItemPatch, string, error) {
			patch, err := buildItemPatch(item, p)
			return patch, mutationAction(p), err
		})
	if txErr != nil {
		return nil, mapItemMutationError(txErr)
	}

	// After the commit, never inside it. Publishing from within the
	// transaction would ship a state that can still roll back, and would hold
	// the row lock across three network calls.
	//
	// Unconditional on which field changed. The indexed item document carries
	// the due date, the note's absence, the lifecycle and more besides, and the
	// refresh rebuilds the whole projection either way, so narrowing this would
	// save nothing and leave the rest stale.
	s.refreshIndex(ctx, p.ProjectUID)

	// LifecycleLive rather than the formation's stored value, which is scoped
	// to the transaction above and not worth hoisting out: this line is only
	// reached when the write committed, and a write only commits when the
	// read-only check at the top of the transaction passed. A checklist that
	// was not live could not have produced this result.
	item := itemToWire(result, model.LifecycleLive)
	return &svc.UpdateItemResult{Item: item, Etag: itemETag(item)}, nil
}

// mutateItem runs the read-validate-write-log sequence one item write is made
// of, and both write routes go through it: the whole sequence is one
// transaction, so a failure partway — most concretely, the activity append
// after the item write succeeds — leaves neither change committed rather than
// an item mutation with no audit trail.
//
// build is handed the item as it stands and returns the patch to apply plus the
// name the activity entry gets. It is where the two routes differ and the only
// place they do; everything around it — the missing checklist, the frozen
// checklist, the stale precondition, the unknown key — is refused identically,
// and in that order, so the two routes cannot drift into answering different
// codes for the same condition.
//
// Returns the updated item and the assignee it had beforehand, the latter
// captured inside the transaction so the caller can decide whether an
// assignment actually changed.
func (s *Service) mutateItem(
	ctx context.Context,
	projectUID, itemKey string,
	ifMatch int64,
	build func(item *model.Item) (port.ItemPatch, string, error),
) (*model.Item, string, error) {
	var result *model.Item
	var prevAssignee string // empty means no prior assignee

	err := s.uow.Do(ctx, func(tx port.Tx) error {
		// Locked for the life of the transaction, because the lifecycle read on
		// the next line is a precondition for a write to a different row. The
		// item's revision guards the item; nothing guards a freeze committing
		// in between, so without the lock this check can be true when it is
		// made and false when the write lands.
		formation, err := tx.Formations().GetByProjectForUpdate(ctx, projectUID)
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

		item, err := tx.Items().GetByKey(ctx, formation.UID, itemKey)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.NewReasonErrorf(domain.ErrNotFound, reasonUnknownItemKey,
					"no item with key %q exists", itemKey)
			}
			return err
		}
		if item.Revision != ifMatch {
			return domain.NewReasonError(domain.ErrVersionMismatch, reasonVersionMismatch)
		}

		prevAssignee = item.Assignee

		patch, action, err := build(item)
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
			Action:       action,
			Before:       activitySummary(item),
			After:        activitySummary(updated),
		}); err != nil {
			return err
		}

		result = updated
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return result, prevAssignee, nil
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

	projectName, projectSlug, parentSlug := s.projectEmailContext(ctx, projectUID)
	formationSlug := projectSlug
	if formationSlug == "" {
		formationSlug = projectUID
	}
	checklistURL := fmt.Sprintf("%s/foundation/formations/%s?project=%s&item=%s",
		s.emailCfg.AdminBaseURL, formationSlug, parentSlug, item.ItemKey)

	var dueDate string
	if item.DueDate != nil {
		dueDate = item.DueDate.Format("2006-01-02")
	}

	subject, html, text, err := email.RenderItemAssigned(email.ItemAssignedData{
		RecipientName: item.Assignee,
		ProjectName:   projectName,
		ItemTitle:     item.Title,
		IsGating:      item.Gate,
		DueDate:       dueDate,
		ChecklistURL:  checklistURL,
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
		GroupID: "formation.item_assigned." + projectUID + "." + item.ItemKey,
	}); sendErr != nil {
		slog.WarnContext(ctx, "item-assigned email: send failed",
			"item_key", item.ItemKey, "assignee", item.Assignee, "error", sendErr)
	}
}

// projectEmailContext returns the project's display name, its own slug (used
// as the formation path segment), and its parent's slug (used as the
// ?project= query parameter in deep links). All three degrade to empty string
// when the project reader is not wired or a lookup fails, so an email can
// still be built with a UID-based URL fallback rather than dropping the send.
func (s *Service) projectEmailContext(ctx context.Context, projectUID string) (name, slug, parentSlug string) {
	if s.projects == nil {
		return "", "", ""
	}
	var err error
	name, err = s.projects.Name(ctx, projectUID)
	if err != nil {
		slog.WarnContext(ctx, "email: could not resolve project name", "project_uid", projectUID, "error", err)
	}
	ref, err := s.projects.GetRef(ctx, projectUID)
	if err != nil {
		slog.WarnContext(ctx, "email: could not resolve project ref", "project_uid", projectUID, "error", err)
		return name, "", ""
	}
	slug = ref.Slug
	if ref.ParentUID != "" {
		parentSlug, err = s.projects.Slug(ctx, ref.ParentUID)
		if err != nil {
			slog.WarnContext(ctx, "email: could not resolve parent slug", "parent_uid", ref.ParentUID, "error", err)
		}
	}
	return name, slug, parentSlug
}

// refreshIndex asks for the project's queue rows to be republished, if anything
// is wired to do that.
//
// One helper for the two places an item write commits, so the nil check and the
// ordering rule live once. It cannot block and cannot fail: see
// Refresher.AfterItemWrite.
func (s *Service) refreshIndex(ctx context.Context, projectUID string) {
	if s.refresher == nil {
		return
	}
	s.refresher.AfterItemWrite(ctx, projectUID)
}

// buildItemPatch validates this route's two fields and turns them into a
// port.ItemPatch. It returns before setting anything on the returned patch once
// it finds a refusal, so a caller never sees a partial patch alongside an error.
func buildItemPatch(item *model.Item, p *svc.UpdateItemPayload) (port.ItemPatch, error) {
	var patch port.ItemPatch

	// Refused rather than applied as a no-op: Update always increments the
	// revision, so an empty body would invalidate every other client's
	// if_match and append an activity entry recording no change.
	if p.Note == nil && p.EvidenceLink == nil {
		return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonNoFieldsToUpdate)
	}

	if p.EvidenceLink != nil {
		if *p.EvidenceLink != "" && !isSafeURL(*p.EvidenceLink) {
			return port.ItemPatch{}, domain.NewReasonError(domain.ErrInvalidRequest, reasonLinkSchemeInvalid)
		}
		patch.EvidenceLink = p.EvidenceLink
	}
	if p.Note != nil {
		patch.Note = p.Note
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
	case p.EvidenceLink != nil:
		return "evidence_link_changed"
	case p.Note != nil:
		return "note_changed"
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
