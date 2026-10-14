// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/application_accepted.html
var applicationAcceptedHTML string

//go:embed templates/application_accepted.txt
var applicationAcceptedTxt string

var (
	applicationAcceptedHTMLTmpl = htmltemplate.Must(htmltemplate.New("application_accepted.html").Parse(applicationAcceptedHTML))
	applicationAcceptedTxtTmpl  = texttemplate.Must(texttemplate.New("application_accepted.txt").Parse(applicationAcceptedTxt))
)

// ApplicationAcceptedData holds the template variables for the acceptance
// notification sent to the submitter.
type ApplicationAcceptedData struct {
	// RecipientName is the submitter's display name. May be empty.
	RecipientName string

	// ProjectName is the proposed project name from the application answers.
	ProjectName string

	// FormationEmail is the formation team's contact address.
	FormationEmail string
}

// RenderApplicationAccepted renders the subject, HTML body and plain-text body
// for the acceptance notification sent to the submitter.
func RenderApplicationAccepted(d ApplicationAcceptedData) (subject, html, text string, err error) {
	subject = "Your application for " + d.ProjectName + " has been accepted"

	var buf bytes.Buffer
	if err = applicationAcceptedHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = applicationAcceptedTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
