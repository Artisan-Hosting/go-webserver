package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// captchaConfig reads the Cap (https://trycap.dev) instance settings from the
// environment. Cap is self-hosted, so CAPTCHA_API_ENDPOINT must point at the
// deployed instance's site path, e.g. "https://cap.example.com/<site-key>/".
// CAPTCHA_SECRET_KEY is the matching secret key created alongside that site
// key in the Cap dashboard. Leaving either unset disables captcha enforcement
// so the contact form keeps working before an instance is provisioned.
func captchaConfig() (endpoint string, secret string, enabled bool) {
	endpoint = strings.TrimSpace(os.Getenv("CAPTCHA_API_ENDPOINT"))
	secret = strings.TrimSpace(os.Getenv("CAPTCHA_SECRET_KEY"))
	enabled = endpoint != "" && secret != ""
	return
}

// captchaConfigHandler exposes the public captcha configuration (whether
// captcha is enabled and, if so, which endpoint the frontend widget should
// talk to) as JSON. The secret key is never returned to the client.
func captchaConfigHandler(w http.ResponseWriter, r *http.Request) {
	endpoint, _, enabled := captchaConfig()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"enabled":  enabled,
		"endpoint": endpoint,
	})
}

// verifyCaptcha calls the Cap instance's siteverify endpoint, matching the
// reCAPTCHA-compatible contract documented at https://trycap.dev/guide/.
func verifyCaptcha(endpoint, secret, token string) (bool, error) {
	verifyURL := strings.TrimRight(endpoint, "/") + "/siteverify"
	body, err := json.Marshal(map[string]string{
		"secret":   secret,
		"response": token,
	})
	if err != nil {
		return false, err
	}

	req, err := http.NewRequest(http.MethodPost, verifyURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()

	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.Success, nil
}
