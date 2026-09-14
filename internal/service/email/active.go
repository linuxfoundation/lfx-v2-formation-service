// Copyright The Linux Foundation and each contributor to LFX.
// SPDX-License-Identifier: MIT

package email

import (
	"bytes"
	_ "embed"
	htmltemplate "html/template"
	texttemplate "text/template"
)

//go:embed templates/active.html
var activeHTML string

//go:embed templates/active.txt
var activeTxt string

var (
	activeHTMLTmpl = htmltemplate.Must(htmltemplate.New("active.html").Parse(activeHTML))
	activeTxtTmpl  = texttemplate.Must(texttemplate.New("active.txt").Parse(activeTxt))
)

// ActiveData holds the template variables for the "project is now Active"
// email sent to all formation grant holders (writers + auditors).
type ActiveData struct {
	// RecipientName is the display name of the recipient, if known.
	// May be empty when only an email address is available.
	RecipientName string

	// ProjectName is the project's display name.
	ProjectName string

	// ProjectURL is a link to the project on LFX.
	ProjectURL string
}

// RenderActive renders the subject, HTML body and plain-text body for the
// "project is now Active" notification sent to formation grant holders.
func RenderActive(d ActiveData) (subject, html, text string, err error) {
	subject = d.ProjectName + " is now Active on LFX"

	var buf bytes.Buffer
	if err = activeHTMLTmpl.Execute(&buf, d); err != nil {
		return
	}
	html = buf.String()

	buf.Reset()
	if err = activeTxtTmpl.Execute(&buf, d); err != nil {
		return
	}
	text = buf.String()
	return
}
