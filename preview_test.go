package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chai2010/webp"
)

func resetPreviewState() {
	defaultPreviewState = newPreviewState()
}

func TestPreviewURLRewriteAndKeys(t *testing.T) {
	normalized, err := normalizePreviewURL(" https://example.com/page?q=1#fragment ")
	if err != nil || normalized != "https://example.com/page?q=1" {
		t.Fatalf("normalized=%q err=%v", normalized, err)
	}
	for _, raw := range []string{"/relative", "file:///tmp/x", ":bad"} {
		if _, err := normalizePreviewURL(raw); err == nil {
			t.Errorf("normalizePreviewURL(%q) succeeded", raw)
		}
	}
	if previewCacheKey(normalized, "a") == previewCacheKey(normalized, "b") {
		t.Fatal("build hash did not affect public key")
	}
	if previewObjectKey(normalized) != previewObjectKey(normalized) {
		t.Fatal("object key is unstable")
	}

	src := []byte(`<html><img class="card" src="remote.jpg" data-server-preview-url="https://example.com/page#x"><img data-server-preview-url="/bad"></html>`)
	got := string(rewriteServerPreviewSources(src, "build"))
	wantKey := previewCacheKey("https://example.com/page", "build")
	if !strings.Contains(got, `/__preview/`+wantKey+`.webp`) || !strings.Contains(got, `data-preview-origin="server"`) {
		t.Fatalf("rewrite=%s", got)
	}
	if !strings.Contains(got, `data-server-preview-url="/bad"`) {
		t.Fatal("invalid target should be left unchanged")
	}
	inserted := setOrInsertImgSrc(`<img alt="x">`, "/new.webp")
	if !strings.Contains(inserted, `src="/new.webp"`) {
		t.Fatalf("insert=%s", inserted)
	}
}

func TestResolveHTMLPathAndRewriteHandler(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(`<img data-server-preview-url="https://example.com">`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "about.html"), []byte("about"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "index.html"), []byte("docs"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/", "/about", "/docs/"} {
		if _, _, ok := resolveHTMLPath(dir, route); !ok {
			t.Errorf("route %s did not resolve", route)
		}
	}
	for _, route := range []string{"/missing", "/../secret", "/about.txt"} {
		if _, _, ok := resolveHTMLPath(dir, route); ok {
			t.Errorf("route %s unexpectedly resolved", route)
		}
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler := htmlPreviewRewriteHandler(dir, fallback, func() string { return "build" })
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "/__preview/") || recorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("response=%d %s %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/", nil),
		httptest.NewRequest(http.MethodGet, "/missing", nil),
	} {
		recorder = httptest.NewRecorder()
		handler(recorder, req)
		if recorder.Code != http.StatusTeapot {
			t.Fatalf("fallback status=%d", recorder.Code)
		}
	}
}

func TestBuildHashAndPreviewSourceIndex(t *testing.T) {
	resetPreviewState()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.html")
	if err := os.WriteFile(path, []byte(`<img data-server-preview-url="https://example.com/a#x"><img data-server-preview-url="bad">`), 0o644); err != nil {
		t.Fatal(err)
	}
	first := computeSiteBuildHash(dir)
	rebuildPreviewSourceIndex(dir, first)
	key := previewCacheKey("https://example.com/a", first)
	if target, ok := previewSourceByHash(key); !ok || target != "https://example.com/a" {
		t.Fatalf("lookup=%q %v", target, ok)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if second := computeSiteBuildHash(dir); second == first {
		t.Fatal("site hash did not change")
	}
}

func TestPreviewMemoryAndDiskCaches(t *testing.T) {
	resetPreviewState()
	original := []byte("data")
	storePreviewInMemory("fresh", original)
	original[0] = 'X'
	data, ok := getFreshPreviewFromMemory("fresh")
	if !ok || string(data) != "data" {
		t.Fatalf("memory=%q %v", data, ok)
	}
	if string(data) != "data" {
		t.Fatal("store did not copy input")
	}
	defaultPreviewState.cacheMu.Lock()
	defaultPreviewState.cacheItems["old"] = previewCacheEntry{data: []byte("old"), expiresAt: time.Now().Add(-time.Second)}
	defaultPreviewState.cacheMu.Unlock()
	if _, ok := getFreshPreviewFromMemory("old"); ok {
		t.Fatal("expired memory entry returned")
	}

	dir := t.TempDir()
	freshPath := filepath.Join(dir, "fresh.webp")
	oldPath := filepath.Join(dir, "old.webp")
	if err := os.WriteFile(freshPath, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldPath, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if data, _, ok := getFreshPreviewFromDisk(freshPath, time.Hour); !ok || string(data) != "fresh" {
		t.Fatalf("fresh disk=%q %v", data, ok)
	}
	if _, _, ok := getFreshPreviewFromDisk(oldPath, time.Hour); ok {
		t.Fatal("expired disk entry returned")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatal("expired disk entry was not removed")
	}

	otherOld := filepath.Join(dir, "other.webp")
	_ = os.WriteFile(otherOld, []byte("old"), 0o644)
	_ = os.Chtimes(otherOld, oldTime, oldTime)
	purgeExpiredPreviewFiles(dir, time.Hour)
	if _, err := os.Stat(otherOld); !os.IsNotExist(err) {
		t.Fatal("purge did not remove expired file")
	}
}

func TestPreviewImageHandlerPaths(t *testing.T) {
	resetPreviewState()
	dir := t.TempDir()
	target := "https://example.com/page"
	source := func(hash string) (string, bool) {
		if hash == "1234567890abcdef" {
			return target, true
		}
		return "", false
	}
	fetchCalls := 0
	handler := previewImageHandlerWithDependencies(dir, source, http.DefaultClient, func(_ *http.Client, got string) ([]byte, error) {
		fetchCalls++
		if got != target {
			t.Fatalf("target=%q", got)
		}
		return []byte("webp-data"), nil
	}, func() time.Time { return time.Unix(1000, 0) })

	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{http.MethodPost, "/__preview/1234567890abcdef.webp", http.StatusMethodNotAllowed},
		{http.MethodGet, "/__preview/x.webp", http.StatusBadRequest},
		{http.MethodGet, "/__preview/ffffffffffffffff.webp", http.StatusNotFound},
	} {
		recorder := httptest.NewRecorder()
		handler(recorder, httptest.NewRequest(tc.method, tc.path, nil))
		if recorder.Code != tc.status {
			t.Errorf("%s status=%d want=%d", tc.path, recorder.Code, tc.status)
		}
	}

	request := httptest.NewRequest(http.MethodGet, "/__preview/1234567890abcdef.webp", nil)
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "webp-data" || fetchCalls != 1 {
		t.Fatalf("fetch response=%d %q calls=%d", recorder.Code, recorder.Body.String(), fetchCalls)
	}
	if _, err := os.Stat(filepath.Join(dir, previewObjectKey(target)+".webp")); err != nil {
		t.Fatal("fetched preview not persisted")
	}
	second := httptest.NewRecorder()
	handler(second, request)
	if fetchCalls != 1 {
		t.Fatal("memory cache missed")
	}

	resetPreviewState()
	third := httptest.NewRecorder()
	handler(third, request)
	if fetchCalls != 1 || third.Body.String() != "webp-data" {
		t.Fatal("disk cache missed")
	}

	resetPreviewState()
	_ = os.Remove(filepath.Join(dir, previewObjectKey(target)+".webp"))
	failing := previewImageHandlerWithDependencies(dir, source, http.DefaultClient, func(*http.Client, string) ([]byte, error) {
		return nil, errors.New("offline")
	}, time.Now)
	redirect := httptest.NewRecorder()
	failing(redirect, request)
	if redirect.Code != http.StatusTemporaryRedirect || redirect.Header().Get("Location") != "/images/featured-placeholder.svg" {
		t.Fatalf("redirect=%d %q", redirect.Code, redirect.Header().Get("Location"))
	}
}

func TestFetchPreviewImageFallsBackAndEncodes(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("User-Agent") != "artisan-studio-preview/1.0" {
			t.Error("missing user agent")
		}
		body := []byte("not an image")
		if calls == 2 {
			body = pngBytes(t, 1200, 1600)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: make(http.Header)}, nil
	})}
	data, err := fetchPreviewImage(client, "https://example.com")
	if err != nil || calls != 2 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	img, err := webp.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() > previewMaxImageWidth || img.Bounds().Dy() > previewMaxImageHeight {
		t.Fatalf("preview too large: %v", img.Bounds())
	}

	badClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	if _, err := fetchPreviewImage(badClient, "https://example.com"); err == nil {
		t.Fatal("expected all-source failure")
	}
}
