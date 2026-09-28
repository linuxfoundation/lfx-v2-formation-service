// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/application_submitted_team.html
var applicationSubmittedTeamHTML string

//go:embed templates/application_submitted_team.txt
var applicationSubmittedTeamTxt string

var (
	applicationSubmittedTeamHTMLTmpl = htmltemplate.Must(htmltemplate.New("application_submitted_team.html").Parse(applicationSubmittedTeamHTML))
	applicationSubmittedTeamTxtTmpl  = texttemplate.Must(texttemplate.New("application_submitted_team.txt").Parse(applicationSubmittedTeamTxt))
)

// ApplicationSubmittedTeamData holds the template variables for the new
// application alert sent to the formation team.
type ApplicationSubmittedTeamData struct {
	// ProjectName is the proposed project name from the application answers.
	ProjectName string

	// ReviewQueueURL is the admin tool link to the application review queue.
	ReviewQueueURL string
}

// RenderApplicationSubmittedTeam renders the subject, HTML body and plain-text
// body for the new-application alert sent to the formation team inbox.
func RenderApplicationSubmittedTeam(d ApplicationSubmittedTeamData) (subject, html, text string, err error) {
	subject = "New project application – " + d.ProjectName

	var buf bytes.Buffer
	if err = applicationSubmittedTeamHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = applicationSubmittedTeamTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
