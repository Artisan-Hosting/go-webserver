package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCaptchaConfigAndHandler(t *testing.T) {
	t.Setenv("CAPTCHA_API_ENDPOINT", " https://cap.example/site/ ")
	t.Setenv("CAPTCHA_SECRET_KEY", " secret ")
	endpoint, secret, enabled := captchaConfig()
	if endpoint != "https://cap.example/site/" || secret != "secret" || !enabled {
		t.Fatalf("config = %q %q %v", endpoint, secret, enabled)
	}
	recorder := httptest.NewRecorder()
	captchaConfigHandler(recorder, httptest.NewRequest(http.MethodGet, "/api/captcha-config", nil))
	if strings.Contains(recorder.Body.String(), "secret") {
		t.Fatal("handler leaked captcha secret")
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil || body["enabled"] != true {
		t.Fatalf("body=%s err=%v", recorder.Body.String(), err)
	}

	t.Setenv("CAPTCHA_SECRET_KEY", "")
	_, _, enabled = captchaConfig()
	if enabled {
		t.Fatal("partial config should be disabled")
	}
}

func TestVerifyCaptcha(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		{"success", `{"success":true}`, true, false},
		{"rejected", `{"success":false}`, false, false},
		{"invalid json", `{`, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/site/siteverify" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var payload map[string]string
				_ = json.NewDecoder(r.Body).Decode(&payload)
				if payload["secret"] != "secret" || payload["response"] != "token" {
					t.Errorf("payload=%v", payload)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.body)), Header: make(http.Header)}, nil
			})}
			got, err := verifyCaptchaWithClient(client, "https://cap.example/site/", "secret", "token")
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("got=%v err=%v", got, err)
			}
		})
	}
	if _, err := verifyCaptcha(":bad", "secret", "token"); err == nil {
		t.Fatal("expected invalid URL error")
	}
}

func TestRelayMailer(t *testing.T) {
	var received EmailPayload
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected mail request")
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"success","message":"queued"}`)), Header: make(http.Header)}, nil
	})}
	payload := EmailPayload{Destination: "a@example.com", Subject: "Subject", Body: "Body"}
	if err := (relayMailer{url: "https://relay.example/send", client: client}).Send(payload); err != nil {
		t.Fatal(err)
	}
	if received != payload {
		t.Fatalf("received=%+v", received)
	}

	failureClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"status":"failure","message":"nope"}`)), Header: make(http.Header)}, nil
	})}
	if err := (relayMailer{url: "https://relay.example/send", client: failureClient}).Send(payload); err == nil {
		t.Fatal("expected relay failure")
	}
	invalidClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{`)), Header: make(http.Header)}, nil
	})}
	if err := (relayMailer{url: "https://relay.example/send", client: invalidClient}).Send(payload); err == nil {
		t.Fatal("expected decode error")
	}
	if err := (relayMailer{url: ":bad", client: http.DefaultClient}).Send(payload); err == nil {
		t.Fatal("expected request construction error")
	}
}
