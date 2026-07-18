package main

import (
	"fmt"
	"strings"

	"github.com/matcornic/hermes"
)

// -----------------------------------------------------
//
//	EMAIL CONSTRUCTORS
//
// -----------------------------------------------------
//
// buildOwnerEmail and buildClientEmail describe emails as plain data
// (hermes.Body{}) — no HTML/CSS lives here. Layout and styling come from
// server/mailtheme.go and server/theme/*.css. See docs/MAIL_THEMING.md
// for how to add another email the same way.

// buildOwnerMessage assembles a plain-text fallback message for the business
// owner's notification email when the client did not supply a pre-rendered
// OwnerMessage. It lists the submitter's contact details followed by their
// project message.
func buildOwnerMessage(data FormData) string {
	parts := make([]string, 0, 6)
	parts = append(parts, fmt.Sprintf("Name: %s", strings.TrimSpace(data.Name)))
	parts = append(parts, fmt.Sprintf("Email: %s", strings.TrimSpace(data.Email)))

	if business := strings.TrimSpace(data.Business); business != "" {
		parts = append(parts, fmt.Sprintf("Business/Brand: %s", business))
	}
	if siteURL := strings.TrimSpace(data.SiteURL); siteURL != "" {
		parts = append(parts, fmt.Sprintf("Current Site URL: %s", siteURL))
	}

	parts = append(parts, "")
	parts = append(parts, "Project details:")
	parts = append(parts, strings.TrimSpace(data.ClientMessage))
	return strings.Join(parts, "\n")
}

// buildOwnerEmail renders the HTML email sent to the business owner
// notifying them of a new consult request: the submitter's name, email, and
// message as a label/value table.
func buildOwnerEmail(data FormData) string {
	dictionary := []hermes.Entry{
		{Key: "Name", Value: strings.TrimSpace(data.Name)},
		{Key: "Email", Value: strings.TrimSpace(data.Email)},
	}
	if business := strings.TrimSpace(data.Business); business != "" {
		dictionary = append(dictionary, hermes.Entry{Key: "Business/Brand", Value: business})
	}
	if siteURL := strings.TrimSpace(data.SiteURL); siteURL != "" {
		dictionary = append(dictionary, hermes.Entry{Key: "Current Site URL", Value: siteURL})
	}
	dictionary = append(dictionary, hermes.Entry{Key: "Message", Value: data.OwnerMessage})

	return renderEmail(hermes.Email{
		Body: hermes.Body{
			Title:      "New Artisan Studios Consult Request",
			Dictionary: dictionary,
		},
	})
}

// buildClientEmail renders the HTML confirmation email sent back to the
// person who submitted the contact form, acknowledging receipt of their
// request.
func buildClientEmail(data FormData) string {
	name := strings.TrimSpace(data.Name)
	if name == "" {
		name = "there"
	}

	return renderEmail(hermes.Email{
		Body: hermes.Body{
			Title: fmt.Sprintf("Thanks, %s.", name),
			Intros: []string{
				"We received your consult request and we will follow up shortly.",
			},
			Outros: []string{
				"A real person from Artisan Studios will review your message and reply with practical next steps. If something is urgent, mention it in your reply and we will prioritize accordingly.",
				"You can reply directly to this email any time.",
			},
		},
	})
}
