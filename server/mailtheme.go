package main

import (
	"strings"

	"github.com/matcornic/hermes"
)

// siteTheme is the single hand-written email layout used by every
// transactional email this server sends. It is the only file in the
// project that contains raw HTML/CSS structure for email — everything else
// (server/theme/*.css, individual emails in email_templates.go) is data:
// CSS values or hermes.Body{} struct literals.
//
// The HTML skeleton below is fixed and must not change per deployment; only
// the CSS spliced in at themeCSSPlaceholder should vary between sites. See
// docs/MAIL_THEMING.md for the full class-name contract that theme CSS
// files are expected to style.
type siteTheme struct {
	// css is the site-specific stylesheet, loaded from the file named by
	// MAIL_THEME_CSS (see mail.go), spliced verbatim into the <style>
	// block of siteHTMLTemplate.
	css string
}

var _ hermes.Theme = siteTheme{}

// Name identifies this theme to Hermes; it has no effect on rendering.
func (t siteTheme) Name() string { return "site" }

// HTMLTemplate returns the Go html/template source Hermes executes to
// produce the HTML email body, with this theme's CSS spliced into the
// <style> block in place of themeCSSPlaceholder.
func (t siteTheme) HTMLTemplate() string {
	return strings.Replace(siteHTMLTemplate, themeCSSPlaceholder, t.css, 1)
}

// PlainTextTemplate returns the Go template Hermes executes (then converts
// via html2text) to produce the plain-text fallback body. It intentionally
// carries no theme CSS since plain-text clients ignore styling.
func (t siteTheme) PlainTextTemplate() string { return siteTextTemplate }

// themeCSSPlaceholder marks the point inside siteHTMLTemplate where a
// theme's CSS is inserted. It must not appear inside a theme CSS file.
const themeCSSPlaceholder = "/*__THEME_CSS__*/"

// siteHTMLTemplate is the fixed HTML skeleton for every email. It is a
// standard table-based layout for broad email-client compatibility, driven
// by the {{.Hermes...}} (engine/product config) and {{.Email.Body...}}
// (per-email content) fields Hermes provides — see
// github.com/matcornic/hermes's Template/Hermes/Body types for the full
// field list. Reskinning a deployment means writing a new CSS file that
// styles the classes below (see docs/MAIL_THEMING.md); this HTML should
// not need to change for that.
const siteHTMLTemplate = `<!DOCTYPE html>
<html lang="en" dir="{{.Hermes.TextDirection}}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{ if .Email.Body.Title }}{{ .Email.Body.Title }}{{ else }}{{ .Hermes.Product.Name }}{{ end }}</title>
  <style type="text/css">
    /* Structural reset. Not theme-controlled: leave these alone when reskinning. */
    body, html { margin:0; padding:0; width:100% !important; }
    table { border-collapse:collapse !important; }
    img { border:0; outline:none; text-decoration:none; }
    a { text-decoration:none; }
    @media only screen and (max-width:600px) {
      .email-card, .email-footer { width:100% !important; }
    }

    /* Theme rules. Everything below this line comes from the active theme CSS file. */
    /*__THEME_CSS__*/
  </style>
</head>
<body>
  <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="email-wrap">
    <tr>
      <td align="center" style="padding:24px;">
        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:680px;">
          <tr>
            <td align="center" class="email-masthead">
              <a href="{{ .Hermes.Product.Link }}" target="_blank">
                {{ if .Hermes.Product.Logo }}
                  <img src="{{ .Hermes.Product.Logo | url }}" alt="{{ .Hermes.Product.Name }}">
                {{ else }}
                  {{ .Hermes.Product.Name }}
                {{ end }}
              </a>
            </td>
          </tr>
          <tr>
            <td>
              <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="email-card">
                <tr><td class="email-brandbar"></td></tr>
                <tr>
                  <td class="email-content-cell">
                    <h1 class="email-heading">{{ if .Email.Body.Title }}{{ .Email.Body.Title }}{{ else }}{{ .Email.Body.Greeting }} {{ .Email.Body.Name }},{{ end }}</h1>

                    {{ with .Email.Body.Intros }}{{ range . }}<p class="email-lead">{{ . }}</p>{{ end }}{{ end }}

                    {{ if ne .Email.Body.FreeMarkdown "" }}
                      {{ .Email.Body.FreeMarkdown.ToHTML }}
                    {{ else }}

                      {{ with .Email.Body.Dictionary }}{{ if gt (len .) 0 }}
                        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="email-dictionary">
                          {{ range . }}
                            <tr>
                              <td class="email-dict-label">{{ .Key }}</td>
                              <td class="email-dict-value">{{ .Value }}</td>
                            </tr>
                          {{ end }}
                        </table>
                      {{ end }}{{ end }}

                      {{ with .Email.Body.Table }}{{ $columns := .Columns }}{{ if gt (len .Data) 0 }}
                        <table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" class="email-table">
                          <tr>
                            {{ range $entry := index .Data 0 }}
                              <th{{ with $columns }}{{ $w := index .CustomWidth $entry.Key }}{{ with $w }} width="{{ . }}"{{ end }}{{ $a := index .CustomAlignment $entry.Key }}{{ with $a }} style="text-align:{{ . }}"{{ end }}{{ end }}>{{ $entry.Key }}</th>
                            {{ end }}
                          </tr>
                          {{ range $row := .Data }}
                            <tr>
                              {{ range $cell := $row }}
                                <td{{ with $columns }}{{ $a := index .CustomAlignment $cell.Key }}{{ with $a }} style="text-align:{{ . }}"{{ end }}{{ end }}>{{ $cell.Value }}</td>
                              {{ end }}
                            </tr>
                          {{ end }}
                        </table>
                      {{ end }}{{ end }}

                      {{ with .Email.Body.Actions }}{{ range . }}
                        {{ if .Instructions }}<p class="email-lead">{{ .Instructions }}</p>{{ end }}
                        {{ if .Button.Text }}
                          <table role="presentation" cellpadding="0" cellspacing="0" border="0" align="center" style="margin:24px auto;">
                            <tr>
                              <td align="center">
                                <a href="{{ .Button.Link }}" target="_blank" class="email-button" style="{{ with .Button.Color }}background-color:{{ . }};{{ end }}{{ with .Button.TextColor }}color:{{ . }};{{ end }}">{{ .Button.Text }}</a>
                              </td>
                            </tr>
                          </table>
                          <p class="email-trouble">{{ $.Hermes.Product.TroubleText | replace "{ACTION}" .Button.Text }}<br><a href="{{ .Button.Link }}">{{ .Button.Link }}</a></p>
                        {{ end }}
                        {{ if .InviteCode }}<p class="email-invite-code">{{ .InviteCode }}</p>{{ end }}
                      {{ end }}{{ end }}

                    {{ end }}

                    {{ with .Email.Body.Outros }}{{ range . }}<p class="email-outro">{{ . }}</p>{{ end }}{{ end }}

                    <p class="email-signature">{{ .Email.Body.Signature }},<br>{{ .Hermes.Product.Name }}</p>
                  </td>
                </tr>
              </table>
            </td>
          </tr>
          <tr>
            <td class="email-footer">
              <a href="{{ .Hermes.Product.Link }}">{{ .Hermes.Product.Name }}</a><br>
              {{ .Hermes.Product.Copyright }}
            </td>
          </tr>
        </table>
      </td>
    </tr>
  </table>
</body>
</html>
`

// siteTextTemplate is a small HTML fragment (not literal plain text) that
// Hermes runs through html2text to produce the plain-text fallback body.
// It carries no classes/CSS since plain-text clients ignore styling.
const siteTextTemplate = `<h2>{{ if .Email.Body.Title }}{{ .Email.Body.Title }}{{ else }}{{ .Email.Body.Greeting }} {{ .Email.Body.Name }},{{ end }}</h2>
{{ with .Email.Body.Intros }}{{ range . }}<p>{{ . }}</p>{{ end }}{{ end }}
{{ if ne .Email.Body.FreeMarkdown "" }}
  {{ .Email.Body.FreeMarkdown.ToHTML }}
{{ else }}
  {{ with .Email.Body.Dictionary }}{{ if gt (len .) 0 }}
    <ul>{{ range . }}<li>{{ .Key }}: {{ .Value }}</li>{{ end }}</ul>
  {{ end }}{{ end }}
  {{ with .Email.Body.Table }}{{ if gt (len .Data) 0 }}
    <table>
      <tr>{{ range $entry := index .Data 0 }}<th>{{ $entry.Key }}</th>{{ end }}</tr>
      {{ range $row := .Data }}<tr>{{ range $cell := $row }}<td>{{ $cell.Value }}</td>{{ end }}</tr>{{ end }}
    </table>
  {{ end }}{{ end }}
  {{ with .Email.Body.Actions }}{{ range . }}
    <p>{{ .Instructions }} {{ if .Button.Link }}{{ .Button.Link }}{{ end }}{{ if .InviteCode }}{{ .InviteCode }}{{ end }}</p>
  {{ end }}{{ end }}
{{ end }}
{{ with .Email.Body.Outros }}{{ range . }}<p>{{ . }}</p>{{ end }}{{ end }}
<p>{{ .Email.Body.Signature }},<br>{{ .Hermes.Product.Name }} - {{ .Hermes.Product.Link }}</p>
<p>{{ .Hermes.Product.Copyright }}</p>
`
