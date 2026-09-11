// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/activating.html
var activatingHTML string

//go:embed templates/activating.txt
var activatingTxt string

var (
	activatingHTMLTmpl = htmltemplate.Must(htmltemplate.New("activating.html").Parse(activatingHTML))
	activatingTxtTmpl  = texttemplate.Must(texttemplate.New("activating.txt").Parse(activatingTxt))
)

// ActivatingData holds the template variables for the "ready to set Active"
// email sent to formation@ when a project's checklist gates are cleared and
// its announcement date is set.
type ActivatingData struct {
	// ProjectName is the project's display name.
	ProjectName string

	// AnnouncementDate is the project's scheduled announcement date (YYYY-MM-DD).
	AnnouncementDate string

	// AdminToolURL is the deep link to the formation in the admin tool.
	// Copy must say "admin tool", never "PCC" (issue #1961 AC).
	AdminToolURL string
}

// RenderActivating renders the subject, HTML body and plain-text body for
// the "ready to set Active" notification sent to formation@.
func RenderActivating(d ActivatingData) (subject, html, text string, err error) {
	subject = "Ready to set Active – " + d.ProjectName

	var buf bytes.Buffer
	if err = activatingHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = activatingTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
