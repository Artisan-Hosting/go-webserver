package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/matcornic/hermes"
)

// EmailPayload matches the required JSON structure for the email service.
type EmailPayload struct {
	Destination string `json:"destination"`
	Subject     string `json:"subject"`
	Body        string `json:"body"`
}

const defaultMailRelayURL = "https://relay.artisanhosting.net/api/sendmail"

type relayMailer struct {
	url    string
	client *http.Client
}

// sendMail posts payload to the Artisan Hosting relay service, which
// performs the actual SMTP delivery. The contact handler logs returned
// errors while preserving its fire-and-forget API response behavior.
func sendMail(payload EmailPayload) error {
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // #nosec G402 -- retained for compatibility with the deployed relay.
		},
	}
	return (relayMailer{url: defaultMailRelayURL, client: client}).Send(payload)
}

func (m relayMailer) Send(payload EmailPayload) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode mail payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, m.url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("create mail request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if m.client == nil {
		m.client = http.DefaultClient
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("send mail request: %w", err)
	}
	defer resp.Body.Close()

	var responseData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseData); err != nil {
		return fmt.Errorf("decode mail response: %w", err)
	}

	if responseData["status"] == "success" {
		fmt.Println("Success:", responseData["message"])
		return nil
	}
	return fmt.Errorf("mail relay failed: %v", responseData["message"])
}

// defaultMailThemeCSS is the theme CSS file used when MAIL_THEME_CSS is not
// set in the environment. See docs/MAIL_THEMING.md for how to point a
// deployment at its own theme instead.
const defaultMailThemeCSS = "theme/default.css"

// hermesEngine lazily builds the package-wide Hermes instance from
// environment config on first use, and reuses it for every email rendered
// afterward. It is safe for concurrent use.
var hermesEngine = sync.OnceValue(newHermesEngine)

// newHermesEngine reads MAIL_THEME_CSS and MAIL_PRODUCT_* from the
// environment (loaded from .env by loadDotEnv, or the real process
// environment) to build a Hermes engine configured for this deployment. A
// site rebrands entirely through these values and a CSS file — no Go code
// changes required.
func newHermesEngine() hermes.Hermes {
	cssPath := strings.TrimSpace(os.Getenv("MAIL_THEME_CSS"))
	if cssPath == "" {
		cssPath = defaultMailThemeCSS
	}
	css, err := os.ReadFile(cssPath)
	if err != nil {
		log.Printf("mail theme: failed to read theme CSS %s, emails will render unstyled: %v", cssPath, err)
	}

	productName := strings.TrimSpace(os.Getenv("MAIL_PRODUCT_NAME"))
	if productName == "" {
		productName = "Artisan Studios"
	}
	productLink := strings.TrimSpace(os.Getenv("MAIL_PRODUCT_LINK"))
	if productLink == "" {
		productLink = "https://www.artisanhosting.net"
	}

	return hermes.Hermes{
		Theme: siteTheme{css: string(css)},
		Product: hermes.Product{
			Name:      productName,
			Link:      productLink,
			Logo:      strings.TrimSpace(os.Getenv("MAIL_PRODUCT_LOGO")),
			Copyright: fmt.Sprintf("© %d %s. All rights reserved.", time.Now().Year(), productName),
		},
	}
}

// renderEmail generates the HTML body for email using the active site
// theme. Generation only fails if the theme template itself is malformed,
// which can't happen at runtime since siteHTMLTemplate is a compile-time
// constant; the fallback below exists purely so a future template edit
// fails loudly (in logs) instead of silently, rather than panicking mail
// delivery.
func renderEmail(email hermes.Email) string {
	h := hermesEngine()
	rendered, err := h.GenerateHTML(email)
	if err != nil {
		log.Printf("mail render error: %v", err)
		return fmt.Sprintf("<pre>%s</pre>", html.EscapeString(fmt.Sprintf("%+v", email.Body)))
	}
	return rendered
}
