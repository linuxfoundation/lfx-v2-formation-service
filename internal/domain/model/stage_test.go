// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

// The stage values are matched as strings against another service's enum, so
// this table is the contract. Every value the project service defines appears
// here, which is what makes a value added upstream show up as a gap rather than
// silently falling to the default.
func TestStageGate(t *testing.T) {
	tests := []struct {
		stage      string
		forming    bool
		lifecycle  Lifecycle
		recognised bool
	}{
		{StageFormationExploratory, true, LifecycleLive, true},
		{StageFormationEngaged, true, LifecycleLive, true},
		{StageFormationOnHold, true, LifecycleLive, true},
		{StageFormationConfidential, true, LifecycleLive, true},

		// Carries the formation prefix but has left formation: it must freeze
		// an existing checklist and must never create one.
		{StageFormationDisengaged, false, LifecycleFrozen, true},

		{StageActive, false, LifecycleCompleted, true},
		{StageArchived, false, LifecycleFrozen, true},

		// Creates nothing and says nothing about lifecycle — a Prospect has no
		// checklist to move. Recognised all the same, so it is not reported as
		// an unreadable stage on every sweep.
		{StageProspect, false, "", true},

		// Not the project service's spelling. Must not create a checklist, and
		// must not be read as having left formation either.
		{"Formation-Engaged", false, "", false},
		{"formation - engaged", false, "", false},
		{"", false, "", false},
		{"Something New Upstream", false, "", false},
	}

	for _, tc := range tests {
		t.Run(tc.stage, func(t *testing.T) {
			if got := FormingStage(tc.stage); got != tc.forming {
				t.Errorf("FormingStage(%q) = %v, want %v", tc.stage, got, tc.forming)
			}

			lifecycle, ok := LifecycleForStage(tc.stage)
			if lifecycle != tc.lifecycle {
				t.Errorf("LifecycleForStage(%q) = %q, want %q", tc.stage, lifecycle, tc.lifecycle)
			}
			// Recognition is a separate answer from the lifecycle itself:
			// Prospect is recognised and still yields none, and only a value
			// this service has not been taught is unrecognised.
			if ok != tc.recognised {
				t.Errorf("LifecycleForStage(%q) recognised = %v, want %v", tc.stage, ok, tc.recognised)
			}
		})
	}
}

// The four stages that create nothing are the ones most likely to be got wrong,
// so they are asserted as a set rather than only row by row.
func TestStagesThatCreateNothing(t *testing.T) {
	for _, stage := range []string{StageProspect, StageActive, StageArchived, StageFormationDisengaged} {
		if FormingStage(stage) {
			t.Errorf("FormingStage(%q) = true, want false", stage)
		}
	}
}
