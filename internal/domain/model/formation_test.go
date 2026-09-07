// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import "testing"

func TestApplyInsertDefaults(t *testing.T) {
	t.Run("an item left unset gets the documented defaults", func(t *testing.T) {
		item := &Item{}

		item.ApplyInsertDefaults()

		if item.Revision != 1 {
			t.Errorf("revision = %d, want 1", item.Revision)
		}
		if item.Status != StatusNotStarted {
			t.Errorf("status = %q, want %q", item.Status, StatusNotStarted)
		}
		// An empty ChecklistType is not a valid value on the wire, so the
		// default has to be applied before the item is ever persisted or
		// served.
		if item.ChecklistType != ChecklistBoth {
			t.Errorf("checklist_type = %q, want %q", item.ChecklistType, ChecklistBoth)
		}
		if item.IsRequired {
			t.Error("is_required = true, want false: items are required only when a template says so")
		}
	})

	t.Run("values the caller set are left alone", func(t *testing.T) {
		item := &Item{
			Revision:      7,
			Status:        StatusDone,
			ChecklistType: ChecklistInternal,
			IsRequired:    true,
		}

		item.ApplyInsertDefaults()

		if item.Revision != 7 {
			t.Errorf("revision = %d, want 7", item.Revision)
		}
		if item.Status != StatusDone {
			t.Errorf("status = %q, want %q", item.Status, StatusDone)
		}
		if item.ChecklistType != ChecklistInternal {
			t.Errorf("checklist_type = %q, want %q", item.ChecklistType, ChecklistInternal)
		}
		if !item.IsRequired {
			t.Error("is_required = false, want true")
		}
	})
}
