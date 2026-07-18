package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chai2010/webp"
)

const (
	// previewMaxImageWidth and previewMaxImageHeight bound the dimensions
	// that fetched link-preview screenshots are downscaled to.
	previewMaxImageWidth  = 1024
	previewMaxImageHeight = 1280
	// previewWebPQuality is the lossy encoding quality used for cached
	// link-preview images.
	previewWebPQuality = 68
	// previewTTL is how long a fetched preview image is considered fresh,
	// both in the in-memory cache and on disk.
	previewTTL = 7 * 24 * time.Hour
)

// defaultPreviewCacheDir is the default on-disk location for persisted
// preview images. It lives under the system temp directory since it's
// purely a disk-backed cache (see purgeExpiredPreviewFiles) that can be
// safely lost on reboot.
var defaultPreviewCacheDir = filepath.Join(os.TempDir(), "artisan-webserver", "previews")

// siteBuildHash holds the current build hash of the static site (see
// computeSiteBuildHash), used to key preview URLs so that a rebuild of the
// static assets invalidates previously generated preview links.
var siteBuildHash atomic.Value

// previewCacheEntry is an in-memory cached preview image plus its
// expiration time.
type previewCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

// previewCache is the in-memory, TTL-based cache of fetched preview images,
// keyed by previewObjectKey(targetURL). It sits in front of the on-disk
// cache to avoid repeated file reads for hot preview links.
var previewCache = struct {
	sync.RWMutex
	items map[string]previewCacheEntry
}{items: make(map[string]previewCacheEntry)}

// previewSourceIndex maps a preview cache hash (previewCacheKey) back to the
// original absolute target URL it was derived from, so that requests to
// /__preview/<hash>.webp can be resolved without re-parsing every HTML page
// on each request. It is rebuilt whenever the static site changes.
var previewSourceIndex = struct {
	sync.RWMutex
	items map[string]string
}{items: make(map[string]string)}

// serverPreviewImgTagRE matches <img> tags carrying a
// data-server-preview-url attribute, which marks images that should be
// proxied and cached server-side rather than loaded directly from their
// origin.
var serverPreviewImgTagRE = regexp.MustCompile(`(?i)<img\b[^>]*data-server-preview-url="([^"]+)"[^>]*>`)

// imgSrcAttrRE matches an existing src="..." attribute within an <img> tag,
// used to replace it in place when rewriting preview image sources.
var imgSrcAttrRE = regexp.MustCompile(`(?i)\s+src="[^"]*"`)

// htmlPreviewRewriteHandler returns an http.HandlerFunc that serves HTML
// files from staticDir with server-side preview <img> tags rewritten to
// point at the local /__preview/ proxy (see rewriteServerPreviewSources).
// Non-HTML requests, or requests for files that can't be resolved to an
// HTML file under staticDir, fall back to the provided fallback handler.
func htmlPreviewRewriteHandler(staticDir string, fallback http.Handler, buildHash func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fallback.ServeHTTP(w, r)
			return
		}

		filePath, info, ok := resolveHTMLPath(staticDir, r.URL.Path)
		if !ok {
			fallback.ServeHTTP(w, r)
			return
		}

		src, err := os.ReadFile(filePath)
		if err != nil {
			fallback.ServeHTTP(w, r)
			return
		}

		rewritten := rewriteServerPreviewSources(src, buildHash())
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, filepath.Base(filePath), info.ModTime(), bytes.NewReader(rewritten))
	}
}

// resolveHTMLPath maps a request path to an HTML file under staticDir,
// handling directory-index requests ("/" and paths ending in "/") and
// extensionless routes (trying "<path>.html"). It returns false if no
// matching HTML file exists within staticDir, guarding against path
// traversal outside of it.
func resolveHTMLPath(staticDir, requestPath string) (string, os.FileInfo, bool) {
	cleaned := path.Clean("/" + requestPath)
	if cleaned == "/" {
		cleaned = "/index.html"
	}
	if strings.HasSuffix(cleaned, "/") {
		cleaned += "index.html"
	}

	candidates := []string{cleaned}
	if filepath.Ext(cleaned) == "" {
		candidates = append(candidates, cleaned+".html")
	}

	base := filepath.Clean(staticDir)
	for _, candidate := range candidates {
		rel := strings.TrimPrefix(candidate, "/")
		fullPath := filepath.Clean(filepath.Join(base, filepath.FromSlash(rel)))
		if !strings.HasPrefix(fullPath, base+string(filepath.Separator)) && fullPath != base {
			continue
		}
		if strings.ToLower(filepath.Ext(fullPath)) != ".html" {
			continue
		}
		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			continue
		}
		return fullPath, info, true
	}

	return "", nil, false
}

// rewriteServerPreviewSources rewrites every data-server-preview-url <img>
// tag in src so its src attribute points at the local /__preview/<hash>.webp
// proxy endpoint instead of the original remote URL, tagging the element
// with data-preview-origin="server" for the frontend's benefit. buildHash is
// mixed into the cache key so that a site rebuild produces fresh preview
// URLs.
func rewriteServerPreviewSources(src []byte, buildHash string) []byte {
	return serverPreviewImgTagRE.ReplaceAllFunc(src, func(tag []byte) []byte {
		matches := serverPreviewImgTagRE.FindSubmatch(tag)
		if len(matches) < 2 {
			return tag
		}

		target, err := normalizePreviewURL(string(matches[1]))
		if err != nil {
			return tag
		}

		hash := previewCacheKey(target, buildHash)
		localPreviewSrc := fmt.Sprintf("/__preview/%s.webp", hash)
		updatedTag := setOrInsertImgSrc(string(tag), localPreviewSrc)
		if !strings.Contains(strings.ToLower(updatedTag), "data-preview-origin=") {
			updatedTag = strings.Replace(updatedTag, "<img", `<img data-preview-origin="server"`, 1)
		}
		return []byte(updatedTag)
	})
}

// setOrInsertImgSrc replaces the src attribute of an <img> tag with src,
// or inserts one immediately before the tag's closing ">" if it doesn't
// already have one.
func setOrInsertImgSrc(tag, src string) string {
	replacement := fmt.Sprintf(` src="%s"`, src)
	if imgSrcAttrRE.MatchString(tag) {
		return imgSrcAttrRE.ReplaceAllString(tag, replacement)
	}

	if idx := strings.Index(tag, ">"); idx != -1 {
		return tag[:idx] + replacement + tag[idx:]
	}
	return tag
}

// normalizePreviewURL parses raw as an absolute http(s) URL and strips its
// fragment, returning an error for anything relative or using another
// scheme. This keeps preview targets restricted to fetchable remote pages.
func normalizePreviewURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if !u.IsAbs() {
		return "", fmt.Errorf("preview URL must be absolute: %s", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported preview scheme: %s", u.Scheme)
	}
	u.Fragment = ""
	return u.String(), nil
}

// previewCacheKey derives the public, URL-safe hash used in
// /__preview/<hash>.webp for a given target URL and site build hash. Mixing
// in buildHash means the public hash changes whenever the static site is
// rebuilt, invalidating stale links.
func previewCacheKey(targetURL, buildHash string) string {
	sum := sha256.Sum256([]byte(buildHash + "|" + targetURL))
	return hex.EncodeToString(sum[:16])
}

// previewObjectKey derives the stable, build-independent cache key used to
// store a fetched preview image in memory and on disk, keyed only by the
// target URL so the same fetched image can be reused across site rebuilds.
func previewObjectKey(targetURL string) string {
	sum := sha256.Sum256([]byte(targetURL))
	return hex.EncodeToString(sum[:16])
}

// computeSiteBuildHash walks staticDir and hashes each file's relative
// path, size, and modification time to produce a short fingerprint of the
// current static site build. It changes whenever any static file is added,
// removed, or modified.
func computeSiteBuildHash(staticDir string) string {
	hasher := sha256.New()
	root := filepath.Clean(staticDir)
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = d.Name()
		}
		fmt.Fprintf(hasher, "%s|%d|%d\n", filepath.ToSlash(rel), info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return hex.EncodeToString(hasher.Sum(nil))[:24]
}

// rebuildPreviewSourceIndex re-scans every HTML file under staticDir for
// data-server-preview-url <img> tags and rebuilds previewSourceIndex so
// that previewSourceByHash can resolve incoming /__preview/ requests. It is
// called at startup and whenever the static site changes.
func rebuildPreviewSourceIndex(staticDir, buildHash string) {
	next := make(map[string]string)
	root := filepath.Clean(staticDir)

	_ = filepath.WalkDir(root, func(filePath string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.ToLower(filepath.Ext(filePath)) != ".html" {
			return nil
		}

		src, readErr := os.ReadFile(filePath)
		if readErr != nil {
			return nil
		}

		matches := serverPreviewImgTagRE.FindAllSubmatch(src, -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			target, parseErr := normalizePreviewURL(string(match[1]))
			if parseErr != nil {
				continue
			}
			next[previewCacheKey(target, buildHash)] = target
		}

		return nil
	})

	previewSourceIndex.Lock()
	previewSourceIndex.items = next
	previewSourceIndex.Unlock()
}

// previewSourceByHash looks up the original target URL for a preview hash
// previously produced by previewCacheKey, as populated by
// rebuildPreviewSourceIndex.
func previewSourceByHash(hash string) (string, bool) {
	previewSourceIndex.RLock()
	target, ok := previewSourceIndex.items[hash]
	previewSourceIndex.RUnlock()
	return target, ok
}

// previewImageHandler returns an http.HandlerFunc serving
// /__preview/<hash>.webp requests. It resolves the hash to a target URL via
// sourceForHash, then serves the corresponding preview image from the
// in-memory cache, the on-disk cache, or by fetching and converting it
// fresh (in that order of preference), persisting newly fetched images to
// both caches. If no source can be resolved or fetched, it falls back to a
// redirect to a static placeholder image.
func previewImageHandler(cacheDir string, sourceForHash func(string) (string, bool)) http.HandlerFunc {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Printf("preview cache dir create failed: %v", err)
	}

	client := &http.Client{Timeout: 12 * time.Second}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		hashPart := strings.TrimPrefix(path.Clean(r.URL.Path), "/__preview/")
		hashPart = strings.TrimSuffix(hashPart, ".webp")
		if hashPart == "" || len(hashPart) < 16 {
			http.Error(w, "invalid preview key", http.StatusBadRequest)
			return
		}

		targetURL, ok := sourceForHash(hashPart)
		if !ok || targetURL == "" {
			http.NotFound(w, r)
			return
		}

		objectKey := previewObjectKey(targetURL)

		if data, ok := getFreshPreviewFromMemory(objectKey); ok {
			servePreviewWebP(w, r, hashPart, time.Now(), data, previewTTL)
			return
		}

		diskPath := filepath.Join(cacheDir, objectKey+".webp")
		if data, modTime, ok := getFreshPreviewFromDisk(diskPath, previewTTL); ok {
			storePreviewInMemory(objectKey, data)
			servePreviewWebP(w, r, hashPart, modTime, data, previewTTL)
			return
		}

		data, fetchErr := fetchPreviewImage(client, targetURL)
		if fetchErr != nil {
			log.Printf("preview fetch failed for %s: %v", targetURL, fetchErr)
			http.Redirect(w, r, "/images/featured-placeholder.svg", http.StatusTemporaryRedirect)
			return
		}

		if writeErr := os.WriteFile(diskPath, data, 0o644); writeErr != nil {
			log.Printf("preview cache write failed for %s: %v", diskPath, writeErr)
		}
		storePreviewInMemory(objectKey, data)
		servePreviewWebP(w, r, hashPart, time.Now(), data, previewTTL)
	}
}

// fetchPreviewImage retrieves a screenshot/preview of targetURL from the
// thum.io screenshot service, trying a cropped full-page capture first and
// falling back to the page's Open Graph image, then downscales and
// re-encodes whichever succeeds as WebP.
func fetchPreviewImage(client *http.Client, targetURL string) ([]byte, error) {
	candidates := []string{
		fmt.Sprintf("https://image.thum.io/get/width/1200/crop/760/noanimate/%s", targetURL),
		fmt.Sprintf("https://image.thum.io/get/ogImage/%s", targetURL),
	}

	for _, endpoint := range candidates {
		req, err := http.NewRequest(http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "artisan-studio-preview/1.0")

		resp, err := client.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			continue
		}

		raw, err := io.ReadAll(io.LimitReader(resp.Body, 15<<20))
		resp.Body.Close()
		if err != nil || len(raw) == 0 {
			continue
		}

		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			continue
		}

		processed := resizeToFit(img, previewMaxImageWidth, previewMaxImageHeight)
		var buf bytes.Buffer
		if err := webp.Encode(&buf, processed, &webp.Options{Quality: previewWebPQuality}); err != nil {
			continue
		}

		return buf.Bytes(), nil
	}

	return nil, fmt.Errorf("no preview source returned a valid image")
}

// getFreshPreviewFromMemory returns the cached preview image for key from
// previewCache if present and not yet expired, evicting it if it has
// expired.
func getFreshPreviewFromMemory(key string) ([]byte, bool) {
	now := time.Now()
	previewCache.RLock()
	item, ok := previewCache.items[key]
	previewCache.RUnlock()
	if !ok || now.After(item.expiresAt) {
		if ok {
			previewCache.Lock()
			delete(previewCache.items, key)
			previewCache.Unlock()
		}
		return nil, false
	}
	return item.data, true
}

// storePreviewInMemory caches data in previewCache under key, setting a
// fresh expiration previewTTL from now. The stored bytes are copied so
// callers may reuse their buffer.
func storePreviewInMemory(key string, data []byte) {
	previewCache.Lock()
	previewCache.items[key] = previewCacheEntry{
		data:      append([]byte(nil), data...),
		expiresAt: time.Now().Add(previewTTL),
	}
	previewCache.Unlock()
}

// getFreshPreviewFromDisk reads a cached preview image from path if it
// exists and is younger than ttl, removing it if it has expired.
func getFreshPreviewFromDisk(path string, ttl time.Duration) ([]byte, time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, time.Time{}, false
	}
	if time.Since(info.ModTime()) > ttl {
		_ = os.Remove(path)
		return nil, time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, time.Time{}, false
	}
	return data, info.ModTime(), true
}

// purgeExpiredPreviewFiles removes cached preview files under cacheDir
// whose modification time is older than ttl. It is run once at startup and
// then periodically to keep the on-disk cache bounded.
func purgeExpiredPreviewFiles(cacheDir string, ttl time.Duration) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}

	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(cacheDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > ttl {
			_ = os.Remove(path)
		}
	}
}

// servePreviewWebP writes data as an image/webp response with a
// Cache-Control max-age derived from ttl, honoring conditional GET
// semantics via http.ServeContent.
func servePreviewWebP(w http.ResponseWriter, r *http.Request, key string, modTime time.Time, data []byte, ttl time.Duration) {
	seconds := int(ttl.Seconds())
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", seconds))
	w.Header().Set("Vary", "Accept")
	http.ServeContent(w, r, key+".webp", modTime, bytes.NewReader(data))
}
