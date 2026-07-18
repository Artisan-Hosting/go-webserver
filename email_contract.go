package main

import "github.com/matcornic/hermes"

// ContactEmail is the fixed contract between the contact-form pipeline and a
// site's email copy. Sites customize the subject and Hermes email data while
// the shared server keeps delivery, theming, and rendering centralized.
type ContactEmail struct {
	Destination string
	Subject     string
	Email       hermes.Email
}

// ContactEmailTemplates supplies the emails sent by contactHandler.
//
// Site-specific builds may replace email_templates.go with their own package
// main file that assigns contactEmailTemplates to a custom implementation.
type ContactEmailTemplates interface {
	ClientEmail(data FormData) ContactEmail
	OwnerEmail(data FormData, ownerDestination string) ContactEmail
}
