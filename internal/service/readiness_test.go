// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package service

import "testing"

func TestIsActivating(t *testing.T) {
	date := "2026-09-01"

	tests := []struct {
		name             string
		gateTotal        int
		gateOutstanding  int
		announcementDate *string
		want             bool
	}{
		{
			name:             "ready: gates cleared and announcement date set",
			gateTotal:        3,
			gateOutstanding:  0,
			announcementDate: &date,
			want:             true,
		},
		{
			name: "a skipped gating item leaves the project not ready: " +
				"GateSummary's outstanding count still includes it, since " +
				"skipped is not done",
			gateTotal:        3,
			gateOutstanding:  1, // the skipped item does not satisfy its gate
			announcementDate: &date,
			want:             false,
		},
		{
			name:             "zero gating items reports not ready, not vacuously ready",
			gateTotal:        0,
			gateOutstanding:  0,
			announcementDate: &date,
			want:             false,
		},
		{
			name:             "clearing the announcement date withdraws readiness even with gates cleared",
			gateTotal:        2,
			gateOutstanding:  0,
			announcementDate: nil,
			want:             false,
		},
		{
			name:             "an empty-string announcement date is treated as unset",
			gateTotal:        2,
			gateOutstanding:  0,
			announcementDate: strPtr(""),
			want:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isActivating(tt.gateTotal, tt.gateOutstanding, tt.announcementDate)
			if got != tt.want {
				t.Errorf("isActivating(%d, %d, %v) = %v, want %v",
					tt.gateTotal, tt.gateOutstanding, tt.announcementDate, got, tt.want)
			}
		})
	}
}

func strPtr(s string) *string { return &s }
