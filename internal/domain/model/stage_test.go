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
		stage     string
		forming   bool
		lifecycle Lifecycle
		known     bool
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
		// checklist to move.
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
			// A stage that yields no lifecycle must report so, or a caller
			// switching on the value would treat "" as a real state.
			if ok != (tc.lifecycle != "") {
				t.Errorf("LifecycleForStage(%q) ok = %v, want %v", tc.stage, ok, tc.lifecycle != "")
			}

			if got := KnownStage(tc.stage); got != tc.known {
				t.Errorf("KnownStage(%q) = %v, want %v", tc.stage, got, tc.known)
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

// Disengaged is the reason these two are separate questions.
func TestFormationPrefixIsNotTheGate(t *testing.T) {
	if !HasFormationPrefix(StageFormationDisengaged) {
		t.Error("HasFormationPrefix(Disengaged) = false, want true")
	}
	if FormingStage(StageFormationDisengaged) {
		t.Error("FormingStage(Disengaged) = true, want false")
	}
	if HasFormationPrefix(StageActive) {
		t.Error("HasFormationPrefix(Active) = true, want false")
	}
}
