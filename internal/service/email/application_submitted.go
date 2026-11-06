// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/application_submitted.html
var applicationSubmittedHTML string

//go:embed templates/application_submitted.txt
var applicationSubmittedTxt string

var (
	applicationSubmittedHTMLTmpl = htmltemplate.Must(htmltemplate.New("application_submitted.html").Parse(applicationSubmittedHTML))
	applicationSubmittedTxtTmpl  = texttemplate.Must(texttemplate.New("application_submitted.txt").Parse(applicationSubmittedTxt))
)

// ApplicationSubmittedData holds the template variables for the application
// submission receipt sent to the submitter.
type ApplicationSubmittedData struct {
	// RecipientName is the submitter's display name. May be empty.
	RecipientName string

	// ProjectName is the proposed project name from the application answers.
	ProjectName string

	// ApplicationURL is the self-serve deep link to the submitted application.
	ApplicationURL string

	// FormationEmail is the formation team's contact address.
	FormationEmail string
}

// RenderApplicationSubmitted renders the subject, HTML body and plain-text
// body for the submission receipt sent to the submitter.
func RenderApplicationSubmitted(d ApplicationSubmittedData) (subject, html, text string, err error) {
	subject = "We received your application – " + d.ProjectName

	var buf bytes.Buffer
	if err = applicationSubmittedHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = applicationSubmittedTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
