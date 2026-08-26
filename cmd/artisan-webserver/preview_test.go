package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"math/bits"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	got := string(rewriteServerPreviewSources(src, "build", nil))
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
	handler := htmlPreviewRewriteHandler(dir, fallback, func() string { return "build" }, nil)
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

// waitFor polls condition until it holds or the test gives up, for the
// background captures the preview endpoints now start instead of blocking.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
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
	var fetchCalls atomic.Int64
	handler := previewImageHandlerWithDependencies(dir, source, http.DefaultClient, nil, func(_ *http.Client, got string) ([]byte, error) {
		fetchCalls.Add(1)
		if got != target {
			t.Errorf("target=%q", got)
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
	statusRequest := httptest.NewRequest(http.MethodGet, "/__preview/1234567890abcdef.json", nil)

	// A miss answers immediately with the placeholder rather than holding the
	// request open for the screenshot service, and starts the capture.
	miss := httptest.NewRecorder()
	handler(miss, request)
	if miss.Code != http.StatusTemporaryRedirect || miss.Header().Get("Location") != previewPlaceholderLightSrc {
		t.Fatalf("miss=%d %q", miss.Code, miss.Header().Get("Location"))
	}
	if miss.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("placeholder redirect is cacheable: %q", miss.Header().Get("Cache-Control"))
	}

	waitFor(t, "the background capture to land", func() bool {
		_, cached := defaultPreviewState.cachedPreview(dir, target)
		return cached
	})
	if _, err := os.Stat(filepath.Join(dir, previewObjectKey(target)+".webp")); err != nil {
		t.Fatal("captured preview not persisted")
	}

	hit := httptest.NewRecorder()
	handler(hit, request)
	if hit.Code != http.StatusOK || hit.Body.String() != "webp-data" {
		t.Fatalf("hit=%d %q", hit.Code, hit.Body.String())
	}

	ready := httptest.NewRecorder()
	handler(ready, statusRequest)
	if body := ready.Body.String(); !strings.Contains(body, `"state":"ready"`) ||
		!strings.Contains(body, `"src":"/__preview/1234567890abcdef.webp"`) {
		t.Fatalf("status=%s", body)
	}
	if ready.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("preview status is cacheable")
	}

	second := httptest.NewRecorder()
	handler(second, request)
	if calls := fetchCalls.Load(); calls != 1 {
		t.Fatalf("fetches=%d, want the capture reused from cache", calls)
	}
	if second.Body.String() != "webp-data" {
		t.Fatal("memory cache missed")
	}

	resetPreviewState()
	third := httptest.NewRecorder()
	handler(third, request)
	if calls := fetchCalls.Load(); calls != 1 || third.Body.String() != "webp-data" {
		t.Fatalf("disk cache missed: fetches=%d body=%q", calls, third.Body.String())
	}

	// A capture that fails leaves the target on the placeholder and says so.
	resetPreviewState()
	_ = os.Remove(filepath.Join(dir, previewObjectKey(target)+".webp"))
	failing := previewImageHandlerWithDependencies(dir, source, http.DefaultClient, nil, func(*http.Client, string) ([]byte, error) {
		return nil, errors.New("offline")
	}, time.Now)
	redirect := httptest.NewRecorder()
	failing(redirect, request)
	if redirect.Code != http.StatusTemporaryRedirect || redirect.Header().Get("Location") != previewPlaceholderLightSrc {
		t.Fatalf("redirect=%d %q", redirect.Code, redirect.Header().Get("Location"))
	}
	waitFor(t, "the failed capture to be recorded", func() bool {
		return defaultPreviewState.isUnavailable(target)
	})

	unavailable := httptest.NewRecorder()
	failing(unavailable, statusRequest)
	if body := unavailable.Body.String(); !strings.Contains(body, `"state":"unavailable"`) {
		t.Fatalf("status=%s", body)
	}

	darkRequest := httptest.NewRequest(http.MethodGet, "/__preview/1234567890abcdef.webp", nil)
	darkRequest.Header.Set("Sec-CH-Prefers-Color-Scheme", "dark")
	darkRedirect := httptest.NewRecorder()
	failing(darkRedirect, darkRequest)
	if darkRedirect.Header().Get("Location") != previewPlaceholderDarkSrc {
		t.Fatalf("dark-theme placeholder=%q", darkRedirect.Header().Get("Location"))
	}

	// A target whose origin never answers 200 must not be screenshotted at
	// all: the probe is what stops a 502 page becoming the preview.
	resetPreviewState()
	var fetched atomic.Bool
	unhealthy := previewImageHandlerWithDependencies(dir, source, http.DefaultClient,
		func(*http.Client, string) error { return errors.New("origin returned HTTP 502") },
		func(*http.Client, string) ([]byte, error) {
			fetched.Store(true)
			return []byte("webp-data"), nil
		}, time.Now)
	probed := httptest.NewRecorder()
	unhealthy(probed, request)
	if probed.Code != http.StatusTemporaryRedirect {
		t.Fatalf("unhealthy origin status=%d", probed.Code)
	}
	waitFor(t, "the screening verdict", func() bool { return defaultPreviewState.isUnavailable(target) })
	if fetched.Load() {
		t.Fatal("captured a screenshot of an unhealthy origin")
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

func TestProbePreviewTargetScreensOrigins(t *testing.T) {
	body := strings.Repeat("<p>real page</p>", 64)
	cases := []struct {
		name         string
		status       int
		contentType  string
		body         string
		wantErr      bool
		wantAttempts int
	}{
		{name: "healthy", status: http.StatusOK, contentType: "text/html; charset=utf-8", body: body, wantAttempts: 1},
		{name: "bad gateway", status: http.StatusBadGateway, contentType: "text/html", body: body, wantErr: true, wantAttempts: previewProbeAttempts},
		{name: "gone", status: http.StatusNotFound, contentType: "text/html", body: body, wantErr: true, wantAttempts: 1},
		{name: "not html", status: http.StatusOK, contentType: "application/json", body: body, wantErr: true, wantAttempts: 1},
		{name: "empty page", status: http.StatusOK, contentType: "text/html", body: "  ", wantErr: true, wantAttempts: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			attempts := 0
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if r.Header.Get("User-Agent") != previewUserAgent {
					t.Error("probe did not identify itself")
				}
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer origin.Close()

			err := probePreviewTarget(origin.Client(), origin.URL)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if attempts != tc.wantAttempts {
				t.Fatalf("attempts=%d want=%d", attempts, tc.wantAttempts)
			}
		})
	}

	if err := probePreviewTarget(http.DefaultClient, "http://127.0.0.1:1/down"); err == nil {
		t.Fatal("expected an unreachable origin to fail screening")
	}

	// A blip on the first attempt must not cost a healthy site its preview.
	flaky := 0
	recovering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flaky++
		if flaky == 1 {
			hijacked, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = hijacked.Close()
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, body)
	}))
	defer recovering.Close()
	if err := probePreviewTarget(recovering.Client(), recovering.URL); err != nil {
		t.Fatalf("a one-off connection drop failed screening: %v", err)
	}
}

func TestPreviewImageIsBlankAndFetchRejectsIt(t *testing.T) {
	blank := image.NewRGBA(image.Rect(0, 0, 800, 600))
	draw.Draw(blank, blank.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	if !previewImageIsBlank(blank) {
		t.Fatal("a flat white capture was not called blank")
	}
	if !previewImageIsBlank(image.NewRGBA(image.Rect(0, 0, 8, 8))) {
		t.Fatal("a capture too small to be a page was not rejected")
	}

	// The same white page with content painted on it is a real capture.
	rendered := image.NewRGBA(blank.Bounds())
	draw.Draw(rendered, rendered.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	draw.Draw(rendered, image.Rect(0, 0, 800, 120), &image.Uniform{color.RGBA{R: 11, G: 19, B: 43, A: 255}}, image.Point{}, draw.Src)
	if previewImageIsBlank(rendered) {
		t.Fatal("a rendered page was called blank")
	}

	var blankPNG bytes.Buffer
	if err := png.Encode(&blankPNG, blank); err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(blankPNG.Bytes())),
			Header:     make(http.Header),
		}, nil
	})}
	if _, err := fetchPreviewImage(client, "https://example.com"); err == nil {
		t.Fatal("a blank capture was accepted")
	}
}

func TestRewriteSubstitutesPlaceholderForUnavailableTargets(t *testing.T) {
	src := []byte(`<img src="/fallback.webp" alt="Preview of example" data-server-preview-url="https://example.com">`)
	got := string(rewriteServerPreviewSources(src, "build", func(target string) previewRenderMode {
		return previewRenderUnavailable
	}))
	for _, want := range []string{
		`src="` + previewPlaceholderLightSrc + `"`,
		`data-preview-state="placeholder"`,
		"data-theme-logo",
		`alt="Preview of example"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("placeholder rewrite missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "/__preview/") {
		t.Fatalf("unavailable target still points at the preview proxy: %s", got)
	}

	healthy := string(rewriteServerPreviewSources(src, "build", func(string) previewRenderMode { return previewRenderReady }))
	if !strings.Contains(healthy, "/__preview/") || !strings.Contains(healthy, `data-preview-state="live"`) {
		t.Fatalf("healthy rewrite=%s", healthy)
	}
}

func TestInvalidatePreviewCacheClearsEverything(t *testing.T) {
	dir := t.TempDir()
	state := newPreviewState()
	state.storeInMemory("key", []byte("stale"))
	state.markUnavailable("https://example.com", "origin returned HTTP 502")
	if err := os.WriteFile(filepath.Join(dir, "stale.webp"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	invalidatePreviewCache(dir, state)

	if _, ok := state.getFreshFromMemory("key"); ok {
		t.Fatal("memory cache survived invalidation")
	}
	if state.isUnavailable("https://example.com") {
		t.Fatal("availability verdicts survived invalidation")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("disk cache survived invalidation: %v %v", entries, err)
	}
}

func TestPreviewWarmerCapturesAndRetries(t *testing.T) {
	dir := t.TempDir()
	staticDir := t.TempDir()
	page := `<img data-server-preview-url="https://up.example.com"><img data-server-preview-url="https://down.example.com">`
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte(page), 0o644); err != nil {
		t.Fatal(err)
	}
	state := newPreviewState()
	state.rebuildSourceIndex(staticDir, "build")

	fetches, recovered := 0, false
	warmer := &previewWarmer{
		state:    state,
		cacheDir: dir,
		client:   http.DefaultClient,
		probe: func(_ *http.Client, target string) error {
			if strings.Contains(target, "down.") && !recovered {
				return errors.New("origin returned HTTP 502")
			}
			return nil
		},
		fetch: func(*http.Client, string) ([]byte, error) {
			fetches++
			return []byte("webp-data"), nil
		},
	}

	warmer.warmAll(context.Background())
	if state.isUnavailable("https://up.example.com") {
		t.Fatal("healthy target marked unavailable")
	}
	if !state.isUnavailable("https://down.example.com") {
		t.Fatal("unhealthy target not marked unavailable")
	}
	if fetches != 1 {
		t.Fatalf("fetches=%d, want only the healthy target captured", fetches)
	}
	if _, err := os.Stat(filepath.Join(dir, previewObjectKey("https://up.example.com")+".webp")); err != nil {
		t.Fatal("warm capture not persisted")
	}

	// A second pass leaves the cached healthy capture alone and re-screens
	// the one that was down, so a site coming back up recovers its preview
	// without a restart.
	recovered = true
	warmer.warmAll(context.Background())
	if fetches != 2 {
		t.Fatalf("fetches=%d, want the recovered target captured and the healthy one skipped", fetches)
	}
	if state.isUnavailable("https://down.example.com") {
		t.Fatal("recovered target still marked unavailable")
	}
}

// errorPageImage draws something shaped like the screenshot service's
// unreachable-site page: a mostly blank sheet with a short dark block near
// the upper left, whose position shifts a little with the URL it names.
func errorPageImage(textWidth int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 1200, 760))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	ink := &image.Uniform{color.RGBA{R: 32, G: 33, B: 36, A: 255}}
	draw.Draw(img, image.Rect(120, 300, 120+textWidth, 340), ink, image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(120, 380, 120+textWidth/2, 400), ink, image.Point{}, draw.Src)
	return img
}

func TestPreviewCaptureFingerprintSeparatesErrorPagesFromScreenshots(t *testing.T) {
	// The same error page naming two different URLs must hash close enough
	// to be recognised as one page.
	first := previewCaptureFingerprint(errorPageImage(420))
	second := previewCaptureFingerprint(errorPageImage(470))
	if distance := bits.OnesCount64(first ^ second); distance > previewErrorCaptureDistance {
		t.Fatalf("two error captures are %d bits apart, want <= %d", distance, previewErrorCaptureDistance)
	}

	// A real screenshot must not. A page's blocks of varying brightness are
	// what a fingerprint keys on, so the fixture is blocky rather than a
	// smooth gradient (which, having no local contrast, hashes like a blank
	// sheet).
	page := image.NewRGBA(image.Rect(0, 0, 1200, 760))
	random := rand.New(rand.NewSource(7))
	for blockY := 0; blockY < 760; blockY += 40 {
		for blockX := 0; blockX < 1200; blockX += 40 {
			shade := uint8(random.Intn(256))
			block := image.Rect(blockX, blockY, blockX+40, blockY+40)
			draw.Draw(page, block, &image.Uniform{color.RGBA{R: shade, G: shade, B: shade, A: 255}}, image.Point{}, draw.Src)
		}
	}
	if distance := bits.OnesCount64(first ^ previewCaptureFingerprint(page)); distance <= previewErrorCaptureDistance {
		t.Fatalf("a real screenshot is only %d bits from the error page", distance)
	}

	if previewCaptureFingerprint(image.NewRGBA(image.Rect(0, 0, 4, 4))) != 0 {
		t.Fatal("an image too small to fingerprint should hash to zero")
	}
}

func TestFetchPreviewImageRejectsUnreachableSiteCaptures(t *testing.T) {
	errorCaptureReference = &previewErrorReference{}
	t.Cleanup(func() { errorCaptureReference = &previewErrorReference{} })

	var requested []string
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requested = append(requested, r.URL.String())
		// The sentinel and the target both come back as the service's
		// error page, naming a different URL each time.
		width := 420
		if strings.Contains(r.URL.String(), "example.com") {
			width = 470
		}
		var body bytes.Buffer
		if err := png.Encode(&body, errorPageImage(width)); err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(&body), Header: make(http.Header)}, nil
	})}

	// Before the reference is learned the check is inert, so a capture that
	// happens to look like the error page is still accepted.
	if _, err := fetchPreviewImage(client, "https://example.com"); err != nil {
		t.Fatalf("capture rejected before the reference was learned: %v", err)
	}

	errorCaptureReference.ensure(client)
	if !strings.Contains(requested[len(requested)-1], previewSentinelURL) {
		t.Fatalf("the reference was not learned from the sentinel: %v", requested)
	}

	_, err := fetchPreviewImage(client, "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "could not reach the site") {
		t.Fatalf("err=%v, want the unreachable-site capture rejected", err)
	}

	// A learned reference must not reject genuine screenshots.
	realClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(pngBytes(t, 1200, 760))),
			Header:     make(http.Header),
		}, nil
	})}
	if _, err := fetchPreviewImage(realClient, "https://example.com"); err != nil {
		t.Fatalf("a real screenshot was rejected: %v", err)
	}
}

func TestPreviewErrorReferenceSurvivesAnUnreachableService(t *testing.T) {
	errorCaptureReference = &previewErrorReference{}
	t.Cleanup(func() { errorCaptureReference = &previewErrorReference{} })

	offline := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	errorCaptureReference.ensure(offline)
	if errorCaptureReference.matches(0) {
		t.Fatal("an unlearned reference must never match")
	}
}

func TestRewriteDefersPendingPreviews(t *testing.T) {
	src := []byte(`<img src="/fallback.webp" alt="Preview of example" data-server-preview-url="https://example.com">`)
	got := string(rewriteServerPreviewSources(src, "build", func(string) previewRenderMode {
		return previewRenderPending
	}))
	hash := previewCacheKey("https://example.com", "build")

	// The tag must resolve from our own disk, not from the screenshot
	// service, while carrying what the page needs to swap in the capture.
	if !strings.Contains(got, `src="`+previewPlaceholderLightSrc+`"`) {
		t.Fatalf("pending tag does not lead with the placeholder: %s", got)
	}
	for _, want := range []string{
		`data-preview-state="placeholder"`,
		`data-preview-src="/__preview/` + hash + `.webp"`,
		`data-preview-status="/__preview/` + hash + `.json"`,
		"data-theme-logo",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("pending tag missing %s: %s", want, got)
		}
	}

	// An unavailable target gets no swap addresses: nothing is coming.
	unavailable := string(rewriteServerPreviewSources(src, "build", func(string) previewRenderMode {
		return previewRenderUnavailable
	}))
	if strings.Contains(unavailable, "data-preview-status") {
		t.Fatalf("unavailable tag invites polling: %s", unavailable)
	}
}

func TestPreviewCaptureEndpointsForceAFreshRender(t *testing.T) {
	endpoints := previewCaptureEndpoints("https://example.com/page", "nonce123")
	if len(endpoints) != 3 {
		t.Fatalf("endpoints=%v", endpoints)
	}
	if strings.Contains(endpoints[0], "nonce123") || strings.Contains(endpoints[0], "wait/") {
		t.Fatalf("the first attempt should be the service's cached capture: %s", endpoints[0])
	}
	retry := endpoints[1]
	if !strings.Contains(retry, fmt.Sprintf("wait/%d/", previewRenderWaitSeconds)) {
		t.Fatalf("the retry does not wait for the page to paint: %s", retry)
	}
	if !strings.HasSuffix(retry, "https://example.com/page?preview=nonce123") {
		t.Fatalf("the retry is not cache-busted: %s", retry)
	}
	if !strings.Contains(endpoints[2], "ogImage") {
		t.Fatalf("endpoints=%v", endpoints)
	}

	withQuery := previewCaptureEndpoints("https://example.com/page?a=1", "n")
	if !strings.HasSuffix(withQuery[1], "?a=1&preview=n") {
		t.Fatalf("existing query mishandled: %s", withQuery[1])
	}

	// The sentinel lookup wants the plain endpoints only.
	if bare := previewCaptureEndpoints("https://example.com", ""); len(bare) != 2 {
		t.Fatalf("bare=%v", bare)
	}
}
