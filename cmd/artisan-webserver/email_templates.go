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
// contactEmailTemplates is the default Artisan Studios implementation. Site
// builds can replace this file with their own package main file that assigns
// this variable to a custom ContactEmailTemplates implementation.
var contactEmailTemplates ContactEmailTemplates = defaultContactEmailTemplates{}

type defaultContactEmailTemplates struct{}

// buildOwnerEmail renders the HTML email sent to the business owner
// notifying them of a new consult request: the submitter's name, email, and
// message as a label/value table.
func buildOwnerEmail(data FormData) string {
	return renderEmail(defaultContactEmailTemplates{}.OwnerEmail(data, "").Email)
}

// buildClientEmail renders the HTML confirmation email sent back to the
// person who submitted the contact form, acknowledging receipt of their
// request.
func buildClientEmail(data FormData) string {
	return renderEmail(defaultContactEmailTemplates{}.ClientEmail(data).Email)
}

func (defaultContactEmailTemplates) OwnerEmail(data FormData, ownerDestination string) ContactEmail {
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

	return ContactEmail{
		Destination: ownerDestination,
		Subject:     fmt.Sprintf("Artisan Studios: New consult request from %s", data.Name),
		Email: hermes.Email{
			Body: hermes.Body{
				Title:      "New Artisan Studios Consult Request",
				Dictionary: dictionary,
			},
		},
	}
}

func (defaultContactEmailTemplates) ClientEmail(data FormData) ContactEmail {
	name := strings.TrimSpace(data.Name)
	if name == "" {
		name = "there"
	}

	return ContactEmail{
		Destination: strings.TrimSpace(data.Email),
		Subject:     "Artisan Studios: We received your consult request",
		Email: hermes.Email{
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
		},
	}
}
