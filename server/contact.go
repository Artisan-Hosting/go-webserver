package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/quotedprintable"
	"net/http"
	"os"
	"strings"
)

// FormData is the JSON body accepted by contactHandler, submitted by the
// site's contact form.
type FormData struct {
	Name          string `json:"name"`
	Email         string `json:"email"`
	Business      string `json:"business"`
	SiteURL       string `json:"site_url"`
	ClientMessage string `json:"ClientMessage"`
	OwnerMessage  string `json:"OwnerMessage"` // Backwards-compatible with prior payloads
	CapToken      string `json:"capToken"`
}

// contactHandler validates and processes a contact-form submission: it
// verifies the captcha (if enabled), decodes a quoted-printable owner
// message when present, fills in sensible defaults for missing fields,
// and sends both a confirmation email to the submitter and a notification
// email to the business owner.
func contactHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var data FormData
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(data.Name) == "" || strings.TrimSpace(data.Email) == "" {
		http.Error(w, "name and email are required", http.StatusBadRequest)
		return
	}

	if endpoint, secret, enabled := captchaConfig(); enabled {
		token := strings.TrimSpace(data.CapToken)
		if token == "" {
			http.Error(w, "captcha verification required", http.StatusBadRequest)
			return
		}
		ok, err := verifyCaptcha(endpoint, secret, token)
		if err != nil {
			log.Println("captcha verification error:", err)
			http.Error(w, "captcha verification unavailable", http.StatusBadGateway)
			return
		}
		if !ok {
			http.Error(w, "captcha verification failed", http.StatusBadRequest)
			return
		}
	}

	if data.OwnerMessage != "" {
		ownerMessageReader := quotedprintable.NewReader(bytes.NewReader([]byte(data.OwnerMessage)))
		decodedOwnerMessage, err := io.ReadAll(ownerMessageReader)
		if err == nil {
			data.OwnerMessage = string(decodedOwnerMessage)
		}
	}

	if strings.TrimSpace(data.ClientMessage) == "" {
		data.ClientMessage = "No project details were provided."
	}

	if strings.TrimSpace(data.OwnerMessage) == "" {
		data.OwnerMessage = buildOwnerMessage(data)
	}

	ownerDestination := strings.TrimSpace(os.Getenv("CONTACT_OWNER_EMAIL"))
	if ownerDestination == "" {
		ownerDestination = "info@artisanhosting.net"
	}

	// Email to the user who submitted the form
	if data.Email != "" {
		sendMail(EmailPayload{
			Destination: data.Email,
			Subject:     "Artisan Studios: We received your consult request",
			Body:        buildClientEmail(data),
		})
	}

	// Email to the business owner
	sendMail(EmailPayload{
		Destination: ownerDestination,
		Subject:     fmt.Sprintf("Artisan Studios: New consult request from %s", data.Name),
		Body:        buildOwnerEmail(data),
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}
