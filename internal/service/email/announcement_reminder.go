// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/announcement_reminder.html
var announcementReminderHTML string

//go:embed templates/announcement_reminder.txt
var announcementReminderTxt string

var (
	announcementReminderHTMLTmpl = htmltemplate.Must(htmltemplate.New("announcement_reminder.html").Parse(announcementReminderHTML))
	announcementReminderTxtTmpl  = texttemplate.Must(texttemplate.New("announcement_reminder.txt").Parse(announcementReminderTxt))
)

// AnnouncementReminderKind distinguishes the two announcement-date reminders.
type AnnouncementReminderKind string

const (
	// ReminderThreeDayWarning is sent when the announcement date is three
	// days away and the project has not yet gone Active.
	ReminderThreeDayWarning AnnouncementReminderKind = "3d"

	// ReminderOverdue is sent when the announcement date has passed and the
	// project is still not Active.
	ReminderOverdue AnnouncementReminderKind = "overdue"
)

// AnnouncementReminderData holds the template variables for an announcement
// reminder email.
type AnnouncementReminderData struct {
	// ProjectName is the project's display name.
	ProjectName string

	// AnnouncementDate is the scheduled date in YYYY-MM-DD format.
	AnnouncementDate string

	// Kind selects which variant to render (3-day or overdue).
	Kind AnnouncementReminderKind

	// AdminToolURL is the deep link to the formation in the admin tool.
	AdminToolURL string
}

// RenderAnnouncementReminder renders the subject, HTML body and plain-text
// body for an announcement-date reminder email.
func RenderAnnouncementReminder(d AnnouncementReminderData) (subject, html, text string, err error) {
	switch d.Kind {
	case ReminderOverdue:
		subject = "Announcement date passed – " + d.ProjectName + " is not yet Active"
	default:
		subject = "3 days to announcement – " + d.ProjectName + " is not yet Active"
	}

	var buf bytes.Buffer
	if err = announcementReminderHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = announcementReminderTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
