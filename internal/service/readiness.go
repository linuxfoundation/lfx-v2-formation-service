// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

// isActivating derives readiness: every item required for Active is done,
// AND an announcement date is set. Neither condition alone is
// sufficient — a checklist with its gates cleared but no announcement date
// yet is not activating, and vice versa.
//
// gateTotal and gateOutstanding come from gateSummaryFromItems (count of
// gating items, and count of those not yet done). Zero gating items must
// report not ready rather than vacuously ready — a checklist with no gating
// items has nothing to have cleared. awaiting_acceptance and skipped both
// fail to satisfy a gate: "not yet done" excludes only status = done, so both
// fall out of this correctly without special-casing either here.
func isActivating(gateTotal, gateOutstanding int, announcementDate *string) bool {
	if gateTotal == 0 {
		return false
	}
	if gateOutstanding > 0 {
		return false
	}
	if announcementDate == nil || *announcementDate == "" {
		return false
	}
	return true
}
