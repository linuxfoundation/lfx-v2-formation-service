// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/application_denied.html
var applicationDeniedHTML string

//go:embed templates/application_denied.txt
var applicationDeniedTxt string

var (
	applicationDeniedHTMLTmpl = htmltemplate.Must(htmltemplate.New("application_denied.html").Parse(applicationDeniedHTML))
	applicationDeniedTxtTmpl  = texttemplate.Must(texttemplate.New("application_denied.txt").Parse(applicationDeniedTxt))
)

// ApplicationDeniedData holds the template variables for the denial
// notification sent to the submitter.
type ApplicationDeniedData struct {
	// RecipientName is the submitter's display name. May be empty.
	RecipientName string

	// ProjectName is the proposed project name from the application answers.
	ProjectName string

	// FormationEmail is the formation team's contact address.
	FormationEmail string
}

// RenderApplicationDenied renders the subject, HTML body and plain-text body
// for the denial notification sent to the submitter.
func RenderApplicationDenied(d ApplicationDeniedData) (subject, html, text string, err error) {
	subject = "Your application for " + d.ProjectName + " was not accepted"

	var buf bytes.Buffer
	if err = applicationDeniedHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = applicationDeniedTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
