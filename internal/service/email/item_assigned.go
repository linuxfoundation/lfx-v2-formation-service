// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

// Package email renders the HTML and plain-text bodies for formation
// notification emails. Each function produces a subject, HTML body and
// plain-text body that the NATS email dispatcher publishes to the email
// service. Template rendering lives here so it is reviewable without reading
// a NATS client.
package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/item_assigned.html
var itemAssignedHTML string

//go:embed templates/item_assigned.txt
var itemAssignedTxt string

var (
	itemAssignedHTMLTmpl = htmltemplate.Must(htmltemplate.New("item_assigned.html").Parse(itemAssignedHTML))
	itemAssignedTxtTmpl  = texttemplate.Must(texttemplate.New("item_assigned.txt").Parse(itemAssignedTxt))
)

// ItemAssignedData holds the template variables for an item-assigned email.
type ItemAssignedData struct {
	// RecipientName is the display name of the item owner being notified.
	// May be empty when only an email address is available.
	RecipientName string

	// ProjectName is the project's display name.
	ProjectName string

	// ItemTitle is the checklist item title.
	ItemTitle string

	// IsGating is true when the item blocks the project going Active.
	IsGating bool

	// DueDate is the item's due date in YYYY-MM-DD format, or empty when
	// none is set.
	DueDate string

	// ChecklistURL is the deep link that lands the recipient directly on
	// the checklist item.
	ChecklistURL string
}

// RenderItemAssigned renders the subject, HTML body and plain-text body for
// the item-assigned notification email.
func RenderItemAssigned(d ItemAssignedData) (subject, html, text string, err error) {
	if d.IsGating {
		subject = "Action required – " + d.ItemTitle + " (" + d.ProjectName + " formation checklist)"
	} else {
		subject = "You have been assigned: " + d.ItemTitle + " (" + d.ProjectName + " formation checklist)"
	}

	var buf bytes.Buffer
	if err = itemAssignedHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = itemAssignedTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
