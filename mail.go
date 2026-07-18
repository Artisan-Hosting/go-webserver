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

// sendMail posts payload to the Artisan Hosting relay service, which
// performs the actual SMTP delivery. Errors are logged rather than
// returned since callers treat outbound mail as fire-and-forget.
func sendMail(payload EmailPayload) {
	url := "https://relay.artisanhosting.net/api/sendmail"
	jsonData, err := json.Marshal(payload)
	if err != nil {
		fmt.Println("Error encoding JSON:", err)
		return
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Println("Error creating request:", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("Error sending request:", err)
		return
	}
	defer resp.Body.Close()

	var responseData map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&responseData); err != nil {
		fmt.Println("Error decoding response:", err)
		return
	}

	if responseData["status"] == "success" {
		fmt.Println("Success:", responseData["message"])
	} else {
		fmt.Println("Failed:", responseData["message"])
	}
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
