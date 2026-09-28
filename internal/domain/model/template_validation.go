// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package model

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// itemKeyPattern is snake_case, enforced rather than trusted. Item keys are
// permanent: template upgrades match on key, so a rekey after any checklist
// exists is a data migration rather than an edit.
var itemKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// dueRulePrefix is the only due rule shape: "announcement-30d" means thirty days
// before the project's announcement date.
const dueRulePrefix = "announcement-"

// ParseDueRule reads "announcement-30d" as thirty days before the announcement.
// Exported so the seed job can refuse an unparseable rule at load time; the
// expander only warns and leaves the date unset, which ships a checklist with
// silently missing dates.
func ParseDueRule(rule string) (int, bool) {
	if !strings.HasPrefix(rule, dueRulePrefix) {
		return 0, false
	}
	days, ok := strings.CutSuffix(strings.TrimPrefix(rule, dueRulePrefix), "d")
	if !ok {
		return 0, false
	}
	offset, err := strconv.Atoi(days)
	if err != nil || offset < 0 {
		return 0, false
	}
	return offset, true
}

// ValidateSections enforces what the content schema cannot express on its own.
// Every rule here is one that costs a data migration if it reaches the database
// and is noticed later. The CLI adds its own check for an empty top-level slice
// before calling this; the API accepts empty sections.
func ValidateSections(sections []TemplateSection) error {
	// Keys are unique across the whole template, not per section: items are
	// matched on key alone at upgrade, and the checklist response is flat
	// enough that two rows sharing a key would collide there too.
	seenItem := make(map[string]string)
	seenSection := make(map[string]bool)

	for _, section := range sections {
		if !itemKeyPattern.MatchString(section.Key) {
			return fmt.Errorf("section key %q is not snake_case", section.Key)
		}
		if seenSection[section.Key] {
			return fmt.Errorf("section key %q appears twice", section.Key)
		}
		seenSection[section.Key] = true

		if section.Title == "" {
			return fmt.Errorf("section %q has no title", section.Key)
		}

		if len(section.Items) == 0 {
			return fmt.Errorf("section %q defines no items", section.Key)
		}

		for _, item := range section.Items {
			if !itemKeyPattern.MatchString(item.Key) {
				return fmt.Errorf("item key %q is not snake_case", item.Key)
			}
			if other, dup := seenItem[item.Key]; dup {
				return fmt.Errorf("item key %q appears in both %q and %q", item.Key, other, section.Key)
			}
			seenItem[item.Key] = section.Key

			if item.Title == "" {
				return fmt.Errorf("item %q has no title", item.Key)
			}
			if err := validateItemSource(item); err != nil {
				return err
			}
			if err := validateItemDisplay(item); err != nil {
				return err
			}

			// Sub-item keys are unique within their item, which is the scope
			// a status patch addresses them in.
			seenSub := make(map[string]bool, len(item.SubItems))
			for _, sub := range item.SubItems {
				if !itemKeyPattern.MatchString(sub.Key) {
					return fmt.Errorf("sub-item key %q is not snake_case", sub.Key)
				}
				if sub.Title == "" {
					return fmt.Errorf("sub-item %q has no title", sub.Key)
				}
				if seenSub[sub.Key] {
					return fmt.Errorf("item %q has sub-item key %q twice", item.Key, sub.Key)
				}
				seenSub[sub.Key] = true
			}
		}
	}
	return nil
}

// validateItemSource keeps status_source and platform_check consistent.
func validateItemSource(item TemplateItem) error {
	switch item.StatusSource {
	case SourcePlatform:
		if item.PlatformCheck == nil {
			return fmt.Errorf("item %q is platform-sourced with no platform_check", item.Key)
		}
		if item.PlatformCheck.ResourceType == "" {
			return fmt.Errorf("item %q has a platform_check with no resource_type", item.Key)
		}
		if item.PlatformCheck.MinCount < 1 {
			return fmt.Errorf("item %q has a platform_check with min_count %d, which nothing can fail",
				item.Key, item.PlatformCheck.MinCount)
		}
	case SourceManual:
		if item.PlatformCheck != nil {
			return fmt.Errorf("item %q is manual but carries a platform_check", item.Key)
		}
	default:
		return fmt.Errorf("item %q has status_source %q, want %q or %q",
			item.Key, item.StatusSource, SourceManual, SourcePlatform)
	}
	return nil
}

// validateItemDisplay checks the two fields that are copied to the row verbatim
// and never corrected downstream, so a bad value here reaches a reader.
func validateItemDisplay(item TemplateItem) error {
	switch item.ChecklistType {
	case "", ChecklistInternal, ChecklistExternal, ChecklistBoth:
	default:
		return fmt.Errorf("item %q has checklist_type %q, want %q, %q or %q",
			item.Key, item.ChecklistType,
			ChecklistInternal, ChecklistExternal, ChecklistBoth)
	}

	if item.DueRule != "" {
		if _, ok := ParseDueRule(item.DueRule); !ok {
			return fmt.Errorf("item %q has due_rule %q, want the form announcement-<n>d", item.Key, item.DueRule)
		}
	}
	return nil
}
