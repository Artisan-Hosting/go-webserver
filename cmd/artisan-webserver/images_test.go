package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chai2010/webp"
)

func pngBytes(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 120, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func resetWebPCache() {
	webpCache.Lock()
	webpCache.items = make(map[string]cachedWebP)
	webpCache.Unlock()
}

func TestOptimizedImageHandlerConvertsCachesAndInvalidates(t *testing.T) {
	resetWebPCache()
	dir := t.TempDir()
	imageDir := filepath.Join(dir, "imgs")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(imageDir, "large.png")
	if err := os.WriteFile(path, pngBytes(t, 800, 400), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fallback", http.StatusTeapot) })
	handler := optimizedImageHandler(dir, fallback)

	req := httptest.NewRequest(http.MethodGet, "/imgs/large.png", nil)
	req.Header.Set("Accept", "image/avif,image/webp")
	recorder := httptest.NewRecorder()
	handler(recorder, req)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("response=%d %s", recorder.Code, recorder.Header())
	}
	decoded, err := webp.Decode(bytes.NewReader(recorder.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Bounds().Size(); got.X != 512 || got.Y != 256 {
		t.Fatalf("converted size=%v", got)
	}
	if recorder.Header().Get("Vary") != "Accept" || !strings.Contains(recorder.Header().Get("Cache-Control"), "31536000") {
		t.Fatal("missing cache headers")
	}

	second := httptest.NewRecorder()
	handler(second, req)
	if !bytes.Equal(recorder.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("cache hit changed encoded output")
	}
	webpCache.RLock()
	if len(webpCache.items) != 1 {
		t.Fatalf("cache entries=%d", len(webpCache.items))
	}
	webpCache.RUnlock()

	if err := os.WriteFile(path, pngBytes(t, 100, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	third := httptest.NewRecorder()
	handler(third, req)
	decoded, err = webp.Decode(bytes.NewReader(third.Body.Bytes()))
	if err != nil || decoded.Bounds().Dx() != 100 {
		t.Fatalf("cache was not invalidated: bounds=%v err=%v", decoded.Bounds(), err)
	}
}

func TestOptimizedImageHandlerFallbacksAndHead(t *testing.T) {
	resetWebPCache()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "imgs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "imgs", "small.png"), pngBytes(t, 10, 5), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })
	handler := optimizedImageHandler(dir, fallback)
	for _, tc := range []struct{ method, path, accept string }{
		{http.MethodPost, "/imgs/small.png", "image/webp"},
		{http.MethodGet, "/imgs/small.png", "image/png"},
		{http.MethodGet, "/imgs/missing.png", "image/webp"},
		{http.MethodGet, "/imgs/file.txt", "image/webp"},
		{http.MethodGet, "/imgs/../../outside.png", "image/webp"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Accept", tc.accept)
		recorder := httptest.NewRecorder()
		handler(recorder, req)
		if recorder.Code != http.StatusTeapot {
			t.Errorf("%s %s status=%d", tc.method, tc.path, recorder.Code)
		}
	}
	head := httptest.NewRequest(http.MethodHead, "/imgs/small.png", nil)
	head.Header.Set("Accept", "image/webp")
	recorder := httptest.NewRecorder()
	handler(recorder, head)
	if recorder.Code != http.StatusOK || recorder.Body.Len() != 0 {
		t.Fatalf("HEAD response=%d body=%d", recorder.Code, recorder.Body.Len())
	}
	if supportsWebP(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("empty Accept should not support WebP")
	}
}

func TestOptimizedImageHandlerHonorsQueryParams(t *testing.T) {
	resetWebPCache()
	dir := t.TempDir()
	imageDir := filepath.Join(dir, "imgs")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(imageDir, "wide.png")
	if err := os.WriteFile(path, pngBytes(t, 1600, 800), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fallback", http.StatusTeapot) })
	handler := optimizedImageHandler(dir, fallback)

	// A single dimension leaves the other axis unconstrained rather than
	// boxing it into the default bounds.
	req := httptest.NewRequest(http.MethodGet, "/imgs/wide.png?w=1200", nil)
	req.Header.Set("Accept", "image/webp")
	recorder := httptest.NewRecorder()
	handler(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("response=%d", recorder.Code)
	}
	decoded, err := webp.Decode(bytes.NewReader(recorder.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Bounds().Size(); got.X != 1200 || got.Y != 600 {
		t.Fatalf("w=1200 only: converted size=%v", got)
	}

	// A default (no-param) request against the same source file must not
	// reuse the ?w=1200 cache entry.
	defaultReq := httptest.NewRequest(http.MethodGet, "/imgs/wide.png", nil)
	defaultReq.Header.Set("Accept", "image/webp")
	defaultRecorder := httptest.NewRecorder()
	handler(defaultRecorder, defaultReq)
	decodedDefault, err := webp.Decode(bytes.NewReader(defaultRecorder.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got := decodedDefault.Bounds().Size(); got.X != 512 || got.Y != 256 {
		t.Fatalf("default: converted size=%v", got)
	}

	webpCache.RLock()
	if len(webpCache.items) != 2 {
		t.Fatalf("expected distinct cache entries per option set, got %d", len(webpCache.items))
	}
	webpCache.RUnlock()
}

func TestParseImageRequestOptions(t *testing.T) {
	get := func(rawQuery string) imageRequestOptions {
		return parseImageRequestOptions(httptest.NewRequest(http.MethodGet, "/imgs/x.png?"+rawQuery, nil))
	}

	if got := get(""); got.maxWidth != maxImageWidth || got.maxHeight != maxImageHeight || got.quality != webpQuality {
		t.Fatalf("no params: got %+v", got)
	}
	if got := get("w=1200"); got.maxWidth != 1200 || got.maxHeight != -1 {
		t.Fatalf("w only: got %+v", got)
	}
	if got := get("h=900"); got.maxWidth != -1 || got.maxHeight != 900 {
		t.Fatalf("h only: got %+v", got)
	}
	if got := get("q=10"); got.quality != minWebPQuality {
		t.Fatalf("q below range: got %+v", got)
	}
	if got := get("q=200"); got.quality != maxWebPQuality {
		t.Fatalf("q above range: got %+v", got)
	}
	if got := get("q=abc"); got.quality != webpQuality {
		t.Fatalf("invalid q: got %+v", got)
	}
	if got := get("w=0&h=-5"); got.maxWidth != maxImageWidth || got.maxHeight != maxImageHeight {
		t.Fatalf("zero/negative w+h should default: got %+v", got)
	}
}

func TestResizeIfNeededUnconstrainedAxis(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 1000, 100))
	if got := resizeIfNeeded(src, -1, -1).Bounds().Size(); got.X != 1000 || got.Y != 100 {
		t.Fatalf("fully unconstrained should be unchanged: got %v", got)
	}
	if got := resizeIfNeeded(src, 500, -1).Bounds().Size(); got.X != 500 || got.Y != 50 {
		t.Fatalf("width-constrained only: got %v", got)
	}
}

func TestOptimizedImageHandlerReconvertsAfterIdleTTL(t *testing.T) {
	resetWebPCache()
	dir := t.TempDir()
	imageDir := filepath.Join(dir, "imgs")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(imageDir, "large.png")
	if err := os.WriteFile(path, pngBytes(t, 800, 400), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fallback", http.StatusTeapot) })
	handler := optimizedImageHandler(dir, fallback)

	req := httptest.NewRequest(http.MethodGet, "/imgs/large.png", nil)
	req.Header.Set("Accept", "image/webp")
	first := httptest.NewRecorder()
	handler(first, req)
	if first.Code != http.StatusOK {
		t.Fatalf("first response=%d", first.Code)
	}

	// Backdate the entry's lastAccessed past imageCacheTTL, simulating an
	// image nobody has requested in a while; a hit updates the key format,
	// so find whatever key resetWebPCache/the handler produced.
	webpCache.Lock()
	if len(webpCache.items) != 1 {
		webpCache.Unlock()
		t.Fatalf("expected exactly one cache entry, got %d", len(webpCache.items))
	}
	for key, entry := range webpCache.items {
		entry.lastAccessed = time.Now().Add(-imageCacheTTL - time.Minute)
		webpCache.items[key] = entry
	}
	webpCache.Unlock()

	second := httptest.NewRecorder()
	handler(second, req)
	if second.Code != http.StatusOK {
		t.Fatalf("second response=%d", second.Code)
	}

	webpCache.RLock()
	defer webpCache.RUnlock()
	if len(webpCache.items) != 1 {
		t.Fatalf("expected the stale entry to be replaced, not duplicated: %d entries", len(webpCache.items))
	}
	for _, entry := range webpCache.items {
		if time.Since(entry.lastAccessed) > time.Minute {
			t.Fatalf("expected lastAccessed to be refreshed by the reconversion, got %v", entry.lastAccessed)
		}
	}
}

func TestOptimizedImageHandlerTouchesLastAccessedOnHit(t *testing.T) {
	resetWebPCache()
	dir := t.TempDir()
	imageDir := filepath.Join(dir, "imgs")
	if err := os.Mkdir(imageDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(imageDir, "large.png")
	if err := os.WriteFile(path, pngBytes(t, 800, 400), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fallback", http.StatusTeapot) })
	handler := optimizedImageHandler(dir, fallback)

	req := httptest.NewRequest(http.MethodGet, "/imgs/large.png", nil)
	req.Header.Set("Accept", "image/webp")
	handler(httptest.NewRecorder(), req)

	webpCache.Lock()
	var key string
	for k, entry := range webpCache.items {
		key = k
		entry.lastAccessed = time.Now().Add(-imageCacheTTL / 2)
		webpCache.items[k] = entry
	}
	webpCache.Unlock()

	handler(httptest.NewRecorder(), req)

	webpCache.RLock()
	defer webpCache.RUnlock()
	if time.Since(webpCache.items[key].lastAccessed) > time.Second {
		t.Fatal("cache hit did not refresh lastAccessed")
	}
}

func TestPruneExpiredImageCache(t *testing.T) {
	cache := newWebPConversionCache()
	cache.items["fresh"] = cachedWebP{data: []byte("a"), lastAccessed: time.Now()}
	cache.items["stale"] = cachedWebP{data: []byte("b"), lastAccessed: time.Now().Add(-2 * time.Hour)}

	pruneExpiredImageCache(cache, time.Hour)

	cache.RLock()
	defer cache.RUnlock()
	if _, ok := cache.items["stale"]; ok {
		t.Fatal("stale entry should have been pruned")
	}
	if _, ok := cache.items["fresh"]; !ok {
		t.Fatal("fresh entry should have survived pruning")
	}
}

func TestResizeToFit(t *testing.T) {
	small := image.NewRGBA(image.Rect(5, 5, 105, 55))
	if got := resizeToFit(small, 512, 256); got != small {
		t.Fatal("in-bounds image should be returned unchanged")
	}
	wide := image.NewRGBA(image.Rect(0, 0, 1000, 100))
	if got := resizeToFit(wide, 500, 500).Bounds().Size(); got.X != 500 || got.Y != 50 {
		t.Fatalf("wide resize=%v", got)
	}
	tall := image.NewRGBA(image.Rect(0, 0, 10, 1000))
	if got := resizeToFit(tall, 500, 100).Bounds().Size(); got.X != 1 || got.Y != 100 {
		t.Fatalf("tall resize=%v", got)
	}
}
