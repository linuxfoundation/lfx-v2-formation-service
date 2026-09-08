// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "strings"

// A project's stage, as the project service defines it.
//
// There is no separate sub-stage field: the formation sub-stages are compound
// values of the one stage attribute, prefixed "Formation - ". These constants
// are the project service's enum verbatim, spaces and casing included, because
// they are matched against values that arrive as strings — a near-miss such as
// "Formation-Engaged" would read as an unknown stage and quietly create nothing.
const (
	StageFormationExploratory  = "Formation - Exploratory"
	StageFormationEngaged      = "Formation - Engaged"
	StageFormationOnHold       = "Formation - On Hold"
	StageFormationDisengaged   = "Formation - Disengaged"
	StageFormationConfidential = "Formation - Confidential"
	StageActive                = "Active"
	StageArchived              = "Archived"
	StageProspect              = "Prospect"
)

// stagePrefix is what makes a stage a formation stage. Matching the prefix is
// not enough on its own to create a checklist — Disengaged carries it too.
const stagePrefix = "Formation - "

// FormingStage reports whether a project at this stage should have a checklist.
//
// Four of the five formation stages qualify. Disengaged does not: it carries the
// same prefix but means the project has left formation, so it freezes a
// checklist that exists rather than bringing one into being. Prospect, Active
// and Archived create nothing.
//
// An unrecognised value creates nothing. That is the safe direction — a project
// whose stage this service cannot read is left alone rather than given a
// checklist on a guess, and the reconcile will pick it up once the value is one
// this knows.
func FormingStage(stage string) bool {
	switch stage {
	case StageFormationExploratory,
		StageFormationEngaged,
		StageFormationOnHold,
		StageFormationConfidential:
		return true
	default:
		return false
	}
}

// LifecycleForStage returns the lifecycle a checklist should hold at this stage,
// and whether the stage says anything about it at all.
//
// Only leaving formation is expressed here. A stage that still forms returns
// live, a project that has become Active completes its checklist, and Archived
// or Disengaged freeze it. An unrecognised stage returns false rather than a
// lifecycle: it must not be read as "no longer forming", because that would
// freeze live checklists on a value this simply has not been taught.
func LifecycleForStage(stage string) (Lifecycle, bool) {
	switch {
	case FormingStage(stage):
		return LifecycleLive, true
	case stage == StageActive:
		return LifecycleCompleted, true
	case stage == StageArchived, stage == StageFormationDisengaged:
		return LifecycleFrozen, true
	default:
		return "", false
	}
}

// KnownStage reports whether the stage is one the project service defines.
// Used to tell "this project has left formation" apart from "this project's
// stage is missing or unreadable", which need opposite handling.
func KnownStage(stage string) bool {
	_, ok := LifecycleForStage(stage)
	return ok || stage == StageProspect
}

// HasFormationPrefix reports whether a stage is any formation stage, Disengaged
// included. Distinct from FormingStage, which asks whether a checklist is due.
func HasFormationPrefix(stage string) bool {
	return strings.HasPrefix(stage, stagePrefix)
}
