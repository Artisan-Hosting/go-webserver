package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerHandlerRoutes(t *testing.T) {
	resetPreviewState()
	resetWebPCache()
	t.Setenv("CAPTCHA_API_ENDPOINT", "")
	t.Setenv("CAPTCHA_SECRET_KEY", "")
	staticDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(staticDir, "imgs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<h1>home</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "imgs", "x.png"), pngBytes(t, 4, 4), 0o644); err != nil {
		t.Fatal(err)
	}
	var mailCount int
	handler := newServerHandler(serverConfig{
		staticDir: staticDir, previewCacheDir: t.TempDir(), buildHash: func() string { return "test" }, hub: newReloadHub(),
	}, serverDependencies{deliverMail: func(EmailPayload) error { mailCount++; return nil }})

	tests := []struct {
		method string
		path   string
		body   string
		status int
		check  string
	}{
		{http.MethodGet, "/", "", http.StatusOK, "home"},
		{http.MethodGet, "/api/captcha-config", "", http.StatusOK, `"enabled":false`},
		{http.MethodGet, "/__preview/not-a-key.webp", "", http.StatusBadRequest, "invalid preview key"},
		{http.MethodPost, "/api/contact", `{"name":"Jane","email":"jane@example.com"}`, http.StatusOK, `"status":"ok"`},
	}
	for _, tc := range tests {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), tc.check) {
			t.Errorf("%s: status=%d body=%q", tc.path, recorder.Code, recorder.Body.String())
		}
	}
	if mailCount != 2 {
		t.Fatalf("mail count=%d", mailCount)
	}

	imageReq := httptest.NewRequest(http.MethodGet, "/imgs/x.png", nil)
	imageReq.Header.Set("Accept", "image/webp")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, imageReq)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("image response=%d %s", recorder.Code, recorder.Header())
	}
}

func TestServerHandlerDependencyDefaultsAndSimError(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte("ok"), 0o644)
	handler := newServerHandler(serverConfig{staticDir: dir, previewCacheDir: t.TempDir(), simError: "503"}, serverDependencies{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/contact", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", recorder.Code)
	}
}

func TestRunRejectsInvalidFlagsWithoutListening(t *testing.T) {
	if err := run(context.Background(), []string{"-not-a-flag"}); err == nil {
		t.Fatal("expected flag parsing error")
	}
}
