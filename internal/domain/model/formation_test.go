// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"testing"
	"time"
)

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
		// Same reason as ChecklistType: "" is outside the manual|platform
		// enum the response declares.
		if item.StatusSource != SourceManual {
			t.Errorf("status_source = %q, want %q", item.StatusSource, SourceManual)
		}
		if item.IsRequired {
			t.Error("is_required = true, want false: items are required only when a template says so")
		}
	})

	t.Run("sub_items becomes an empty slice, not nil", func(t *testing.T) {
		item := &Item{}

		item.ApplyInsertDefaults()

		// The column is NOT NULL DEFAULT '[]'; a nil slice would marshal to
		// JSON null instead.
		if item.SubItems == nil {
			t.Error("sub_items = nil, want an empty slice")
		}
		if len(item.SubItems) != 0 {
			t.Errorf("sub_items has %d entries, want 0", len(item.SubItems))
		}
	})

	t.Run("values the caller set are left alone", func(t *testing.T) {
		item := &Item{
			Revision:      7,
			Status:        StatusDone,
			ChecklistType: ChecklistInternal,
			StatusSource:  SourcePlatform,
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
		if item.StatusSource != SourcePlatform {
			t.Errorf("status_source = %q, want %q", item.StatusSource, SourcePlatform)
		}
		if !item.IsRequired {
			t.Error("is_required = false, want true")
		}
	})
}

func TestTemplateApplyUpsertDefaults(t *testing.T) {
	t.Run("an unset state becomes draft", func(t *testing.T) {
		tpl := &Template{Name: "standard", Version: 1}

		tpl.ApplyUpsertDefaults()

		// "" is outside the draft|published|archived vocabulary, and a
		// notnull column with no default tag would persist it.
		if tpl.State != TemplateDraft {
			t.Errorf("state = %q, want %q", tpl.State, TemplateDraft)
		}
		if tpl.Sections == nil {
			t.Error("sections = nil, want an empty slice")
		}
	})

	t.Run("an explicit state is preserved", func(t *testing.T) {
		tpl := &Template{State: TemplatePublished}

		tpl.ApplyUpsertDefaults()

		if tpl.State != TemplatePublished {
			t.Errorf("state = %q, want %q", tpl.State, TemplatePublished)
		}
	})

	// The immutability guard releases a version that is neither published nor
	// dated. Published-with-no-date satisfies both halves of that, so it reads
	// as never published and its content becomes editable in place — which is
	// what this default exists to prevent, not a cosmetic completeness fix.
	t.Run("publishing without a timestamp is dated rather than left open", func(t *testing.T) {
		tpl := &Template{State: TemplatePublished}

		before := time.Now().UTC()
		tpl.ApplyUpsertDefaults()

		if tpl.PublishedAt == nil {
			t.Fatal("published_at = nil on a published template, which reads to the immutability guard as never published")
		}
		if tpl.PublishedAt.Before(before.Add(-time.Minute)) {
			t.Errorf("published_at = %v, want a time at or after %v", tpl.PublishedAt, before)
		}
	})

	t.Run("an authored publication time is not overwritten", func(t *testing.T) {
		authored := time.Date(2025, 6, 17, 9, 0, 0, 0, time.UTC)
		tpl := &Template{State: TemplatePublished, PublishedAt: &authored}

		tpl.ApplyUpsertDefaults()

		if tpl.PublishedAt == nil || !tpl.PublishedAt.Equal(authored) {
			t.Errorf("published_at = %v, want the authored %v", tpl.PublishedAt, authored)
		}
	})

	t.Run("a draft stays undated, so its content stays editable", func(t *testing.T) {
		tpl := &Template{State: TemplateDraft}

		tpl.ApplyUpsertDefaults()

		if tpl.PublishedAt != nil {
			t.Errorf("published_at = %v on a draft, want nil — dating it would freeze content that is still meant to change",
				tpl.PublishedAt)
		}
	})
}

func TestRequiresWriterOrDefault(t *testing.T) {
	yes, no := true, false

	cases := []struct {
		name     string
		authored *bool
		want     bool
	}{
		// Omitted must not read as false: the value drives the access
		// elevation prompt, so false silently suppresses it.
		{"omitted defaults to true", nil, true},
		{"an authored false is preserved", &no, false},
		{"an authored true is preserved", &yes, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ti := &TemplateItem{RequiresWriter: tc.authored}

			if got := ti.RequiresWriterOrDefault(); got != tc.want {
				t.Errorf("RequiresWriterOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}
