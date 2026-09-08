// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import (
	"net/url"
	"strings"
)

// isSafeURL reports whether rawURL uses an http or https scheme. Mirrors
// lfx-v2-committee-service's internal/service/committee_notification_handler.go,
// which uses this to keep unsafe schemes (e.g. javascript:) out of clickable
// email links. evidence_link feeds the checklist's Quick Links panel, a
// different rendering surface but the same risk.
func isSafeURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	return scheme == "http" || scheme == "https"
}
