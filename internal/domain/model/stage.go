// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

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

// FormationStages lists the stages worth asking the project service for, which
// is what the reconcile sweep filters on.
//
// Wider than FormingStage on purpose. Disengaged creates no checklist, but a
// project that has just become Disengaged has one to freeze, and asking only for
// the stages that create would leave it out of the answer entirely — the sweep
// would then never see the project whose checklist it needs to freeze. Being in
// this list means "visible to the sweep", not "gets a checklist"; the gate
// decides the second, and it refuses Disengaged.
//
// Active and Archived are deliberately absent. They also freeze or complete a
// checklist, but there are far more of them than there are formation projects,
// and the sweep reaches them the other way: by naming the projects it already
// holds a checklist for.
func FormationStages() []string {
	return []string{
		StageFormationExploratory,
		StageFormationEngaged,
		StageFormationOnHold,
		StageFormationDisengaged,
		StageFormationConfidential,
	}
}

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
// and whether the stage is one this service recognises.
//
// The two answers are separate because three cases have to be told apart, and
// collapsing any two of them produces a wrong behaviour:
//
//   - a recognised stage with a lifecycle to hold — forming stays live, Active
//     completes the checklist, Archived and Disengaged freeze it
//   - a recognised stage that implies nothing, which is Prospect: it has no
//     checklist to move, and that is ordinary rather than notable
//   - an unrecognised value, which must be reported and otherwise left alone
//
// An unrecognised stage must never be read as "no longer forming": that would
// freeze live checklists over a value this has simply not been taught. And
// Prospect must not be reported as unrecognised, or the log line that exists to
// surface a genuine upstream change is buried under the most common stage on the
// platform.
func LifecycleForStage(stage string) (Lifecycle, bool) {
	switch {
	case FormingStage(stage):
		return LifecycleLive, true
	case stage == StageActive:
		return LifecycleCompleted, true
	case stage == StageArchived, stage == StageFormationDisengaged:
		return LifecycleFrozen, true
	case stage == StageProspect:
		// Recognised, with nothing to say about a lifecycle.
		return "", true
	default:
		return "", false
	}
}
