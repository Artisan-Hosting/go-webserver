package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func contactRequest(t *testing.T, body any) *http.Request {
	t.Helper()
	var reader io.Reader
	if raw, ok := body.(string); ok {
		reader = strings.NewReader(raw)
	} else {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
		reader = &buf
	}
	return httptest.NewRequest(http.MethodPost, "/api/contact", reader)
}

func TestContactHandlerValidationAndDefaults(t *testing.T) {
	t.Setenv("CAPTCHA_API_ENDPOINT", "")
	t.Setenv("CAPTCHA_SECRET_KEY", "")
	t.Setenv("CONTACT_OWNER_EMAIL", "owner@example.com")

	var sent []EmailPayload
	handler := contactHandlerWithDependencies("", verifyCaptcha, func(payload EmailPayload) error {
		sent = append(sent, payload)
		return nil
	})

	tests := []struct {
		name   string
		req    *http.Request
		status int
	}{
		{"wrong method", httptest.NewRequest(http.MethodGet, "/api/contact", nil), http.StatusMethodNotAllowed},
		{"bad json", contactRequest(t, "{"), http.StatusBadRequest},
		{"missing name", contactRequest(t, FormData{Email: "jane@example.com"}), http.StatusBadRequest},
		{"missing email", contactRequest(t, FormData{Name: "Jane"}), http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler(recorder, tt.req)
			if recorder.Code != tt.status {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tt.status, recorder.Body.String())
			}
		})
	}
	if len(sent) != 0 {
		t.Fatalf("validation failures sent %d emails", len(sent))
	}

	recorder := httptest.NewRecorder()
	handler(recorder, contactRequest(t, FormData{Name: " Jane ", Email: " jane@example.com "}))
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"status":"ok"}` {
		t.Fatalf("unexpected success response: %d %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	if len(sent) != 2 {
		t.Fatalf("sent %d emails, want 2", len(sent))
	}
	if sent[0].Destination != "jane@example.com" || sent[1].Destination != "owner@example.com" {
		t.Fatalf("destinations = %q, %q", sent[0].Destination, sent[1].Destination)
	}
	if !strings.Contains(sent[1].Body, "No project details were provided.") {
		t.Fatalf("owner email did not contain default message")
	}
}

func TestContactHandlerCaptchaBranches(t *testing.T) {
	t.Setenv("CAPTCHA_API_ENDPOINT", "https://captcha.example/site")
	t.Setenv("CAPTCHA_SECRET_KEY", "secret")
	valid := FormData{Name: "Jane", Email: "jane@example.com", CapToken: "token"}

	tests := []struct {
		name       string
		data       FormData
		verify     captchaVerifier
		wantStatus int
	}{
		{"missing token", FormData{Name: "Jane", Email: "jane@example.com"}, func(_, _, _ string) (bool, error) { return true, nil }, http.StatusBadRequest},
		{"rejected", valid, func(_, _, _ string) (bool, error) { return false, nil }, http.StatusBadRequest},
		{"unavailable", valid, func(_, _, _ string) (bool, error) { return false, errors.New("offline") }, http.StatusBadGateway},
		{"accepted", valid, func(endpoint, secret, token string) (bool, error) {
			if endpoint != "https://captcha.example/site" || secret != "secret" || token != "token" {
				t.Fatalf("unexpected captcha arguments")
			}
			return true, nil
		}, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			contactHandlerWithDependencies("", tt.verify, func(EmailPayload) error { return nil })(recorder, contactRequest(t, tt.data))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status=%d, want %d: %s", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestContactHandlerQuotedPrintableAndDeliveryFailure(t *testing.T) {
	t.Setenv("CAPTCHA_API_ENDPOINT", "")
	t.Setenv("CAPTCHA_SECRET_KEY", "")
	var calls int
	handler := contactHandlerWithDependencies("", verifyCaptcha, func(payload EmailPayload) error {
		calls++
		if calls == 2 && !strings.Contains(payload.Body, "Hello = world") {
			t.Fatalf("owner message was not decoded: %s", payload.Body)
		}
		return errors.New("relay down")
	})
	recorder := httptest.NewRecorder()
	handler(recorder, contactRequest(t, FormData{
		Name: "Jane", Email: "jane@example.com", ClientMessage: "Details", OwnerMessage: "Hello =3D world",
	}))
	if recorder.Code != http.StatusOK || calls != 2 {
		t.Fatalf("status=%d calls=%d", recorder.Code, calls)
	}
}

func TestContactHandlerSimulatedErrors(t *testing.T) {
	for mode, status := range map[string]int{"500": 500, "503": 503, "429": 429} {
		t.Run(mode, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			contactHandlerWithDependencies(mode, nil, nil)(recorder, contactRequest(t, FormData{}))
			if recorder.Code != status {
				t.Fatalf("status=%d, want %d", recorder.Code, status)
			}
			if mode == "429" && recorder.Header().Get("Retry-After") != "30" {
				t.Fatal("missing Retry-After")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	contactHandlerWithDependencies("timeout", nil, nil)(recorder, httptest.NewRequest(http.MethodPost, "/api/contact", nil).WithContext(ctx))
}

func TestContactHandlerConnectionDrop(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	writer := &hijackRecorder{ResponseRecorder: httptest.NewRecorder(), conn: serverConn}
	contactHandlerWithDependencies("drop", nil, nil)(writer, contactRequest(t, FormData{}))
	_ = clientConn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := clientConn.Read(make([]byte, 1)); err == nil {
		t.Fatal("expected closed in-memory connection")
	}
}

type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (r *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return r.conn, bufio.NewReadWriter(bufio.NewReader(r.conn), bufio.NewWriter(r.conn)), nil
}
