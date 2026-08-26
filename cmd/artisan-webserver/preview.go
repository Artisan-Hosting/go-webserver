package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"math/bits"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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
	// previewBrowserTTL is how long a browser may reuse a preview image
	// without revalidating. It is deliberately far shorter than previewTTL:
	// the server drops its own cache on restart, and a week-long browser
	// cache would keep a bad capture on screen long after the server fixed
	// it.
	previewBrowserTTL = time.Hour
	// previewUserAgent identifies our outbound preview requests, both to
	// the screenshot service and to the origins we screen.
	previewUserAgent = "artisan-studio-preview/1.0"
	// previewRenderWaitSeconds is how long the screenshot service is asked
	// to let a page paint before photographing it. Client-rendered pages
	// come back blank without it.
	previewRenderWaitSeconds = 6
)

// Origin screening. A screenshot is only worth taking of a page that is
// actually serving a page: capturing a 502 leaves the error screen pinned to
// the site until the cache expires, which is what these checks exist to
// prevent.
const (
	// previewProbeReadLimit bounds how much of an origin's response body is
	// read while screening it.
	previewProbeReadLimit = 256 << 10
	// previewProbeMinBodyBytes is the smallest response body we accept as a
	// real page. Error pages served with a 200 are usually far smaller.
	previewProbeMinBodyBytes = 512
	// previewProbeTimeout bounds one screening request, independently of
	// the client's own timeout, so a slow origin fails fast enough to leave
	// room for a retry.
	previewProbeTimeout = 10 * time.Second
	// previewProbeAttempts is how many times an origin is screened before
	// it is written off. Demoting a healthy client site to a placeholder
	// over one dropped connection is worse than the stale screenshot this
	// screening exists to prevent, so a transient failure gets a second
	// chance.
	previewProbeAttempts = 2
	// previewProbeRetryPause separates those attempts.
	previewProbeRetryPause = 750 * time.Millisecond
)

// Blank-capture screening. A screenshot taken before a client-rendered page
// paints is a single flat color, and is worse than no screenshot at all.
const (
	// previewMinImageDimension rejects captures too small to be a page.
	previewMinImageDimension = 64
	// previewBlankSampleGrid is the number of sample points taken per axis
	// when deciding whether a capture is flat.
	previewBlankSampleGrid = 64
	// previewBlankChannelTolerance is the per-channel 8-bit difference at
	// which two sampled pixels still count as the same color.
	previewBlankChannelTolerance = 6
	// previewBlankUniformRatio is the share of sampled pixels that must
	// match the reference color before a capture is called blank. It sits
	// this high so that a sparse but genuinely rendered page still passes.
	previewBlankUniformRatio = 0.995
)

// Unreachable-capture screening. When the screenshot service itself cannot
// reach a site, it hands back a picture of its browser's "This site can't be
// reached" page. That is a real image of a real page, so neither the origin
// probe (which reaches the site fine from here) nor the blank check rejects
// it — but it is not a preview of anyone's website.
const (
	// previewSentinelURL is a host that can never resolve: RFC 2606 reserves
	// .invalid for exactly this. Asking the screenshot service for it is how
	// we learn what its failure looks like, rather than hard-coding a
	// fingerprint that goes stale the day they restyle the page.
	previewSentinelURL = "https://unreachable.artisan-preview-sentinel.invalid"
	// previewErrorCaptureDistance is how many of the fingerprint's 64 bits
	// may differ before a capture stops counting as that error page. Two
	// error captures naming different URLs hash identically; the nearest
	// real screenshot we have measured sits 12 bits away.
	previewErrorCaptureDistance = 6
	// previewSentinelTTL is how long a learned reference is reused.
	previewSentinelTTL = 24 * time.Hour
)

// Background warming. Previews are captured up front rather than on the
// first visitor's request, so the HTML rewrite knows which targets are
// healthy and can substitute the logo placeholder for the ones that aren't.
const (
	// previewWarmConcurrency bounds how many targets are screened and
	// captured at once.
	previewWarmConcurrency = 4
	// previewWarmInterval is how often the warmer re-runs, picking up
	// targets that were unreachable earlier and refreshing expired ones.
	previewWarmInterval = 30 * time.Minute
	// previewWarmRequestTimeout bounds a single screening or capture
	// request made off the request path. It is looser than the request-path
	// client's timeout because nobody is waiting on it.
	previewWarmRequestTimeout = 25 * time.Second
)

// Placeholder artwork shown in place of a preview whose origin is down or
// whose capture came back blank. The light theme takes the dark-ink lockup
// and vice versa; the pairing matches logoByTheme in static/js/include.js,
// which re-points these images when the visitor's theme changes.
const (
	previewPlaceholderLightSrc = "/imgs/artisan-studios__lockup__dark__2048w.webp?w=640&h=480&q=60"
	previewPlaceholderDarkSrc  = "/imgs/artisan-studios__lockup__light__2048w.webp?w=640&h=480&q=60"
)

// defaultPreviewCacheDir is the default on-disk location for persisted
// preview images. It lives under the system temp directory since it's
// purely a disk-backed cache (see purgeExpiredPreviewFiles) that can be
// safely lost on reboot.
var defaultPreviewCacheDir = filepath.Join(os.TempDir(), "artisan-webserver", "previews")

// previewCacheEntry is an in-memory cached preview image plus its
// expiration time.
type previewCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

// previewAvailability records the outcome of the last attempt to screen and
// capture one preview target.
type previewAvailability struct {
	ok        bool
	reason    string
	checkedAt time.Time
}

type previewState struct {
	cacheMu    sync.RWMutex
	cacheItems map[string]previewCacheEntry
	sourceMu   sync.RWMutex
	sources    map[string]string
	statusMu   sync.RWMutex
	statuses   map[string]previewAvailability
	inflightMu sync.Mutex
	inflight   map[string]struct{}
}

func newPreviewState() *previewState {
	return &previewState{
		cacheItems: make(map[string]previewCacheEntry),
		sources:    make(map[string]string),
		statuses:   make(map[string]previewAvailability),
		inflight:   make(map[string]struct{}),
	}
}

// defaultPreviewState backs compatibility helpers. Each constructed server
// receives its own state through serverConfig.
var defaultPreviewState = newPreviewState()

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
func htmlPreviewRewriteHandler(staticDir string, fallback http.Handler, buildHash func() string, renderMode func(string) previewRenderMode) http.HandlerFunc {
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

		rewritten := rewriteServerPreviewSources(src, buildHash(), renderMode)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		// Asking for the color-scheme hint here is what lets a preview that
		// falls back mid-flight redirect to the right lockup; browsers that
		// don't send it get the light-theme one, matching the site default.
		w.Header().Set("Accept-CH", "Sec-CH-Prefers-Color-Scheme")
		http.ServeContent(w, r, filepath.Base(filePath), info.ModTime(), bytes.NewReader(rewritten))
	}
}

// resolveHTMLPath maps a request path to an HTML file under staticDir,
// handling directory-index requests ("/" and paths ending in "/") and
// extensionless routes (trying "<path>.html"). It returns false if no
// matching HTML file exists within staticDir, guarding against path
// traversal outside of it.
func resolveHTMLPath(staticDir, requestPath string) (string, os.FileInfo, bool) {
	directoryRequest := strings.HasSuffix(requestPath, "/")
	cleaned := path.Clean("/" + requestPath)
	if cleaned == "/" {
		cleaned = "/index.html"
	} else if directoryRequest {
		cleaned += "/index.html"
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
//
// Only a target with a capture already cached is pointed at /__preview/.
// Everything else gets the lockup as its src — marked so CSS letterboxes it
// and include.js's theme swap picks the light or dark artwork — because a
// tag that resolves instantly from our own disk is worth more to the page
// than one that waits on a screenshot service. A target still being captured
// also carries the addresses the page needs to swap the real screenshot in
// when it lands.
func rewriteServerPreviewSources(src []byte, buildHash string, renderMode func(string) previewRenderMode) []byte {
	if renderMode == nil {
		renderMode = func(string) previewRenderMode { return previewRenderReady }
	}
	return serverPreviewImgTagRE.ReplaceAllFunc(src, func(tag []byte) []byte {
		matches := serverPreviewImgTagRE.FindSubmatch(tag)
		if len(matches) < 2 {
			return tag
		}

		target, err := normalizePreviewURL(string(matches[1]))
		if err != nil {
			return tag
		}

		updatedTag := string(tag)
		hash := previewCacheKey(target, buildHash)
		switch renderMode(target) {
		case previewRenderReady:
			updatedTag = setOrInsertImgSrc(updatedTag, fmt.Sprintf("/__preview/%s.webp", hash))
			updatedTag = insertImgAttribute(updatedTag, "data-preview-state", `data-preview-state="live"`)
		case previewRenderPending:
			updatedTag = setOrInsertImgSrc(updatedTag, previewPlaceholderLightSrc)
			updatedTag = insertImgAttribute(updatedTag, "data-preview-state", `data-preview-state="placeholder"`)
			updatedTag = insertImgAttribute(updatedTag, "data-theme-logo", "data-theme-logo")
			updatedTag = insertImgAttribute(updatedTag, "data-preview-src",
				fmt.Sprintf(`data-preview-src="/__preview/%s.webp"`, hash))
			updatedTag = insertImgAttribute(updatedTag, "data-preview-status",
				fmt.Sprintf(`data-preview-status="/__preview/%s.json"`, hash))
		default:
			updatedTag = setOrInsertImgSrc(updatedTag, previewPlaceholderLightSrc)
			updatedTag = insertImgAttribute(updatedTag, "data-preview-state", `data-preview-state="placeholder"`)
			updatedTag = insertImgAttribute(updatedTag, "data-theme-logo", "data-theme-logo")
		}
		updatedTag = insertImgAttribute(updatedTag, "data-preview-origin", `data-preview-origin="server"`)
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

// insertImgAttribute adds attr just after the opening "<img" of tag unless
// an attribute called name is already present, so a hand-written attribute
// in the source HTML always wins over the one we would inject.
func insertImgAttribute(tag, name, attr string) string {
	if strings.Contains(strings.ToLower(tag), strings.ToLower(name)) {
		return tag
	}
	return strings.Replace(tag, "<img", "<img "+attr, 1)
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
	defaultPreviewState.rebuildSourceIndex(staticDir, buildHash)
}

func (state *previewState) rebuildSourceIndex(staticDir, buildHash string) {
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

	state.sourceMu.Lock()
	state.sources = next
	state.sourceMu.Unlock()
	log.Printf("preview index: rebuilt with %d entr(ies) for build %s", len(next), buildHash)
}

// previewSourceByHash looks up the original target URL for a preview hash
// previously produced by previewCacheKey, as populated by
// rebuildPreviewSourceIndex.
func previewSourceByHash(hash string) (string, bool) {
	return defaultPreviewState.sourceByHash(hash)
}

func (state *previewState) sourceByHash(hash string) (string, bool) {
	state.sourceMu.RLock()
	target, ok := state.sources[hash]
	state.sourceMu.RUnlock()
	return target, ok
}

// targets returns every distinct preview target URL currently indexed.
func (state *previewState) targets() []string {
	state.sourceMu.RLock()
	seen := make(map[string]struct{}, len(state.sources))
	targets := make([]string, 0, len(state.sources))
	for _, target := range state.sources {
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		targets = append(targets, target)
	}
	state.sourceMu.RUnlock()
	return targets
}

// markAvailable records that targetURL screened clean and produced a usable
// capture.
func (state *previewState) markAvailable(targetURL string) {
	state.statusMu.Lock()
	state.statuses[targetURL] = previewAvailability{ok: true, checkedAt: time.Now()}
	state.statusMu.Unlock()
}

// markUnavailable records why targetURL cannot be previewed right now, which
// is what makes the HTML rewrite substitute the logo placeholder for it.
func (state *previewState) markUnavailable(targetURL, reason string) {
	state.statusMu.Lock()
	previous, existed := state.statuses[targetURL]
	state.statuses[targetURL] = previewAvailability{ok: false, reason: reason, checkedAt: time.Now()}
	state.statusMu.Unlock()
	if !existed || previous.ok || previous.reason != reason {
		log.Printf("preview: %s unavailable, showing placeholder (%s)", targetURL, reason)
	}
}

// availability reports the last recorded verdict for targetURL, and whether
// any verdict has been recorded at all.
func (state *previewState) availability(targetURL string) (previewAvailability, bool) {
	state.statusMu.RLock()
	status, ok := state.statuses[targetURL]
	state.statusMu.RUnlock()
	return status, ok
}

// previewRenderMode says how a preview <img> should be written into the HTML
// being served right now.
type previewRenderMode int

const (
	// previewRenderReady: a validated screenshot is cached, so the tag can
	// point straight at it.
	previewRenderReady previewRenderMode = iota
	// previewRenderPending: nothing cached yet. The tag gets the lockup and
	// the addresses it needs to swap the screenshot in once the capture
	// lands. Holding the response open for a screenshot service instead
	// would put seconds of someone else's infrastructure on our page load.
	previewRenderPending
	// previewRenderUnavailable: the target failed screening. The lockup is
	// the final answer until the warmer finds it healthy again.
	previewRenderUnavailable
)

// renderMode decides how targetURL should be rendered. A known-bad target
// gets the placeholder even when an older capture is still cached: a site
// that is down should not be shown as though it were up.
func (state *previewState) renderMode(cacheDir, targetURL string) previewRenderMode {
	if state.isUnavailable(targetURL) {
		return previewRenderUnavailable
	}
	if _, ok := state.cachedPreview(cacheDir, targetURL); ok {
		return previewRenderReady
	}
	return previewRenderPending
}

// cachedPreview returns a fresh cached capture for targetURL from memory or
// disk, promoting a disk hit into memory on the way past.
func (state *previewState) cachedPreview(cacheDir, targetURL string) ([]byte, bool) {
	objectKey := previewObjectKey(targetURL)
	if data, ok := state.getFreshFromMemory(objectKey); ok {
		return data, true
	}
	if cacheDir == "" {
		return nil, false
	}
	diskPath := filepath.Join(cacheDir, objectKey+".webp")
	if data, _, ok := getFreshPreviewFromDisk(diskPath, previewTTL); ok {
		state.storeInMemory(objectKey, data)
		return data, true
	}
	return nil, false
}

// beginCapture claims the right to capture targetURL, reporting false if
// another capture for it is already running. Without this, a page with six
// uncaptured cards would ask the screenshot service for the same picture
// once per visitor.
func (state *previewState) beginCapture(targetURL string) bool {
	state.inflightMu.Lock()
	defer state.inflightMu.Unlock()
	if state.inflight == nil {
		state.inflight = make(map[string]struct{})
	}
	if _, running := state.inflight[targetURL]; running {
		return false
	}
	state.inflight[targetURL] = struct{}{}
	return true
}

// endCapture releases the claim taken by beginCapture.
func (state *previewState) endCapture(targetURL string) {
	state.inflightMu.Lock()
	delete(state.inflight, targetURL)
	state.inflightMu.Unlock()
}

// captureInBackground starts a capture for targetURL and returns
// immediately, so no visitor's request ever waits on a screenshot service.
func (state *previewState) captureInBackground(
	client *http.Client,
	probe func(*http.Client, string) error,
	fetch func(*http.Client, string) ([]byte, error),
	cacheDir, targetURL string,
) {
	if !state.beginCapture(targetURL) {
		return
	}
	go func() {
		defer state.endCapture(targetURL)
		if _, err := state.capturePreview(client, probe, fetch, cacheDir, targetURL); err != nil {
			log.Printf("preview: background capture of %s failed: %v", targetURL, err)
		}
	}()
}

// isUnavailable reports whether targetURL is known-bad. A target that has
// never been screened is not treated as unavailable: the /__preview/
// endpoint will screen it on demand and fall back if it has to.
func (state *previewState) isUnavailable(targetURL string) bool {
	status, ok := state.availability(targetURL)
	return ok && !status.ok
}

// previewImageHandler returns an http.HandlerFunc serving the /__preview/
// endpoints: <hash>.webp for the image itself and <hash>.json for its
// state. It resolves the hash to a target URL via sourceForHash and answers
// from cache. A miss never blocks — the capture starts in the background and
// the request is answered immediately, with the lockup placeholder for the
// image and a "pending" verdict for the status.
func previewImageHandler(cacheDir string, sourceForHash func(string) (string, bool)) http.HandlerFunc {
	return previewImageHandlerWithDependencies(
		cacheDir,
		sourceForHash,
		&http.Client{Timeout: 12 * time.Second},
		probePreviewTarget,
		fetchPreviewImage,
		time.Now,
	)
}

func previewImageHandlerWithDependencies(
	cacheDir string,
	sourceForHash func(string) (string, bool),
	client *http.Client,
	probe func(*http.Client, string) error,
	fetch func(*http.Client, string) ([]byte, error),
	now func() time.Time,
) http.HandlerFunc {
	return previewImageHandlerWithStateDependencies(cacheDir, sourceForHash, client, probe, fetch, now, defaultPreviewState)
}

func previewImageHandlerWithStateDependencies(
	cacheDir string,
	sourceForHash func(string) (string, bool),
	client *http.Client,
	probe func(*http.Client, string) error,
	fetch func(*http.Client, string) ([]byte, error),
	now func() time.Time,
	state *previewState,
) http.HandlerFunc {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Printf("preview cache dir create failed: %v", err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		requested := strings.TrimPrefix(path.Clean(r.URL.Path), "/__preview/")
		statusRequest := strings.HasSuffix(requested, ".json")
		hashPart := strings.TrimSuffix(strings.TrimSuffix(requested, ".json"), ".webp")
		if hashPart == "" || len(hashPart) < 16 {
			http.Error(w, "invalid preview key", http.StatusBadRequest)
			return
		}

		targetURL, ok := sourceForHash(hashPart)
		if !ok || targetURL == "" {
			http.NotFound(w, r)
			return
		}

		if data, cached := state.cachedPreview(cacheDir, targetURL); cached {
			if statusRequest {
				writePreviewStatus(w, "ready", fmt.Sprintf("/__preview/%s.webp", hashPart))
				return
			}
			servePreviewWebP(w, r, hashPart, now(), data, previewBrowserTTL)
			return
		}

		if state.isUnavailable(targetURL) {
			if statusRequest {
				writePreviewStatus(w, "unavailable", "")
				return
			}
			servePreviewPlaceholder(w, r)
			return
		}

		// Nothing cached: start the capture and answer now. The page is
		// already showing the lockup and will swap the screenshot in.
		state.captureInBackground(client, probe, fetch, cacheDir, targetURL)
		if statusRequest {
			writePreviewStatus(w, "pending", "")
			return
		}
		servePreviewPlaceholder(w, r)
	}
}

// writePreviewStatus answers a /__preview/<hash>.json request. The response
// is never cached: its whole purpose is to report a state that is about to
// change.
func writePreviewStatus(w http.ResponseWriter, state, src string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	body := fmt.Sprintf(`{"state":%q}`, state)
	if src != "" {
		body = fmt.Sprintf(`{"state":%q,"src":%q}`, state, src)
	}
	_, _ = io.WriteString(w, body)
}

// capturePreview screens targetURL's origin and, only if it answers like a
// healthy page, captures a screenshot of it. Either outcome is recorded in
// the availability index; a successful capture is written to both the
// in-memory and on-disk caches.
func (state *previewState) capturePreview(
	client *http.Client,
	probe func(*http.Client, string) error,
	fetch func(*http.Client, string) ([]byte, error),
	cacheDir, targetURL string,
) ([]byte, error) {
	if probe != nil {
		if err := probe(client, targetURL); err != nil {
			state.markUnavailable(targetURL, err.Error())
			return nil, err
		}
	}

	data, err := fetch(client, targetURL)
	if err != nil {
		state.markUnavailable(targetURL, err.Error())
		return nil, err
	}

	if cacheDir != "" {
		diskPath := filepath.Join(cacheDir, previewObjectKey(targetURL)+".webp")
		if writeErr := os.WriteFile(diskPath, data, 0o644); writeErr != nil {
			log.Printf("preview cache write failed for %s: %v", diskPath, writeErr)
		} else {
			log.Printf("preview cache: stored %s to disk (%s, %d bytes)", targetURL, diskPath, len(data))
		}
	}
	state.storeInMemory(previewObjectKey(targetURL), data)
	state.markAvailable(targetURL)
	return data, nil
}

// probePreviewTarget checks that targetURL is worth screenshotting: it must
// answer 200 with an HTML body of a plausible size. Screenshotting a 502,
// a redirect to a parking page, or an empty response is what leaves an
// error screen pinned to the portfolio until the cache turns over.
//
// A failure that could be the network rather than the site — a dropped
// connection, a timeout, a 5xx — is retried once before the target is
// written off; a definite answer like a 404 or a non-HTML body is not.
func probePreviewTarget(client *http.Client, targetURL string) error {
	var err error
	for attempt := 1; attempt <= previewProbeAttempts; attempt++ {
		if attempt > 1 {
			log.Printf("preview: re-screening %s after %v", targetURL, err)
			time.Sleep(previewProbeRetryPause)
		}
		var retryable bool
		retryable, err = probePreviewTargetOnce(client, targetURL)
		if err == nil || !retryable {
			return err
		}
	}
	return err
}

// probePreviewTargetOnce runs a single screening request, reporting whether
// a failure is worth retrying.
func probePreviewTargetOnce(client *http.Client, targetURL string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), previewProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", previewUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := client.Do(req)
	if err != nil {
		return true, fmt.Errorf("origin unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode >= 500, fmt.Errorf("origin returned HTTP %d", resp.StatusCode)
	}

	contentType := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Type")))
	if contentType != "" &&
		!strings.HasPrefix(contentType, "text/html") &&
		!strings.HasPrefix(contentType, "application/xhtml+xml") {
		return false, fmt.Errorf("origin served %q, not HTML", contentType)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, previewProbeReadLimit))
	if err != nil {
		return true, fmt.Errorf("origin body unreadable: %w", err)
	}
	if len(bytes.TrimSpace(body)) < previewProbeMinBodyBytes {
		return false, fmt.Errorf("origin body too small (%d bytes)", len(body))
	}
	return false, nil
}

// previewCaptureEndpoints lists the screenshot-service URLs tried for a
// target, in the order they are attempted:
//
//  1. The plain capture, which the service serves from its own cache. Fast,
//     free, and right most of the time.
//  2. A forced fresh render that waits for the page to paint. The wait is
//     what rescues a client-rendered page the service otherwise photographs
//     before its JavaScript runs; the nonce is what gets past the service's
//     cache, which is the part that actually matters. Measured against a
//     page that captures blank: the plain URL returns an identical
//     5,402-byte blank however long a wait is requested, and stays blank
//     with a nonce alone — only nonce and wait together produce the real
//     178KB page. Reserving this for a capture that already came back
//     unusable keeps the cost (~8s, and a real page load for the site being
//     photographed) off the common path.
//  3. The page's Open Graph image, if it has one.
//
// An empty nonce omits step 2, which is what the sentinel lookup wants.
func previewCaptureEndpoints(targetURL, nonce string) []string {
	endpoints := []string{
		fmt.Sprintf("https://image.thum.io/get/width/1200/crop/760/noanimate/%s", targetURL),
	}
	if nonce != "" {
		endpoints = append(endpoints, fmt.Sprintf(
			"https://image.thum.io/get/wait/%d/width/1200/crop/760/noanimate/%s",
			previewRenderWaitSeconds, previewNoncedURL(targetURL, nonce)))
	}
	return append(endpoints, fmt.Sprintf("https://image.thum.io/get/ogImage/%s", targetURL))
}

// previewNoncedURL appends a throwaway query parameter to targetURL so the
// screenshot service treats it as a page it has not photographed before.
func previewNoncedURL(targetURL, nonce string) string {
	separator := "?"
	if strings.Contains(targetURL, "?") {
		separator = "&"
	}
	return targetURL + separator + "preview=" + nonce
}

// previewNonce produces the cache-busting value. It is a variable so tests
// can make capture endpoints predictable.
var previewNonce = func() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

// fetchPreviewImage retrieves a screenshot/preview of targetURL from the
// thum.io screenshot service, then downscales and re-encodes the first
// usable capture as WebP. A capture is unusable if it decodes to a flat
// color — a page photographed before its JavaScript painted — or if it is a
// picture of the screenshot service's own "site can't be reached" page.
// Either way the caller falls back to the placeholder.
func fetchPreviewImage(client *http.Client, targetURL string) ([]byte, error) {
	rejected := ""
	for _, endpoint := range previewCaptureEndpoints(targetURL, previewNonce()) {
		img, err := fetchCaptureImage(client, endpoint)
		if err != nil {
			continue
		}

		if previewImageIsBlank(img) {
			rejected = "capture came back blank"
			log.Printf("preview: discarding blank capture from %s", endpoint)
			continue
		}

		if errorCaptureReference.matches(previewCaptureFingerprint(img)) {
			rejected = "screenshot service could not reach the site"
			log.Printf("preview: discarding unreachable-site capture from %s", endpoint)
			continue
		}

		processed := resizeToFit(img, previewMaxImageWidth, previewMaxImageHeight)
		var buf bytes.Buffer
		if err := webp.Encode(&buf, processed, &webp.Options{Quality: previewWebPQuality}); err != nil {
			continue
		}

		return buf.Bytes(), nil
	}

	if rejected != "" {
		return nil, errors.New(rejected)
	}
	return nil, errors.New("no preview source returned a valid image")
}

// fetchCaptureImage GETs one screenshot-service endpoint and decodes the
// image it returns.
func fetchCaptureImage(client *http.Client, endpoint string) (image.Image, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", previewUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("screenshot service returned HTTP %d", resp.StatusCode)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 15<<20))
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("empty capture body: %w", err)
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return img, nil
}

// previewErrorReference remembers what the screenshot service hands back for
// a host that cannot be reached, so captures of that page can be recognised
// whatever URL they name. It is learned from previewSentinelURL rather than
// hard-coded, so a restyle of that page does not silently disable the check.
type previewErrorReference struct {
	mu        sync.RWMutex
	hash      uint64
	known     bool
	learnedAt time.Time
}

// errorCaptureReference is process-wide: every capture path consults it, and
// the warmer keeps it current.
var errorCaptureReference = &previewErrorReference{}

// ensure learns the reference fingerprint unless a fresh one is already
// held. Failure is silent and simply leaves the check inert — a screenshot
// service we cannot reach at all is not one that can hand us an error page.
func (ref *previewErrorReference) ensure(client *http.Client) {
	ref.mu.RLock()
	fresh := ref.known && time.Since(ref.learnedAt) < previewSentinelTTL
	ref.mu.RUnlock()
	if fresh {
		return
	}

	img, err := fetchCaptureImage(client, previewCaptureEndpoints(previewSentinelURL, "")[0])
	if err != nil {
		log.Printf("preview: could not learn the unreachable-site capture: %v", err)
		return
	}
	hash := previewCaptureFingerprint(img)

	ref.mu.Lock()
	ref.hash, ref.known, ref.learnedAt = hash, true, time.Now()
	ref.mu.Unlock()
	log.Printf("preview: unreachable-site captures fingerprint as %016x", hash)
}

// matches reports whether hash is close enough to the learned reference to
// be the same page.
func (ref *previewErrorReference) matches(hash uint64) bool {
	ref.mu.RLock()
	defer ref.mu.RUnlock()
	return ref.known && bits.OnesCount64(ref.hash^hash) <= previewErrorCaptureDistance
}

// previewCaptureFingerprint reduces img to a 9x8 grid of average
// luminances and emits one bit per horizontal neighbour pair — a difference
// hash. It describes layout rather than detail, so two captures of the same
// error page hash identically even though each names a different URL, while
// any real screenshot lands far away.
func previewCaptureFingerprint(img image.Image) uint64 {
	const columns, rows = 9, 8
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < columns || height < rows {
		return 0
	}

	var cells [rows][columns]float64
	for row := 0; row < rows; row++ {
		top := bounds.Min.Y + row*height/rows
		bottom := bounds.Min.Y + (row+1)*height/rows
		for column := 0; column < columns; column++ {
			left := bounds.Min.X + column*width/columns
			right := bounds.Min.X + (column+1)*width/columns
			cells[row][column] = averageLuminance(img, left, top, right, bottom)
		}
	}

	var hash uint64
	for row := 0; row < rows; row++ {
		for column := 0; column < columns-1; column++ {
			hash <<= 1
			if cells[row][column] > cells[row][column+1] {
				hash |= 1
			}
		}
	}
	return hash
}

// averageLuminance averages the perceived brightness of a cell, sampling it
// rather than reading every pixel so the cost stays flat across image sizes.
func averageLuminance(img image.Image, left, top, right, bottom int) float64 {
	const samplesPerAxis = 8
	stepX := max(1, (right-left)/samplesPerAxis)
	stepY := max(1, (bottom-top)/samplesPerAxis)

	total, count := 0.0, 0
	for y := top; y < bottom; y += stepY {
		for x := left; x < right; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			total += 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return total / float64(count)
}

// previewImageIsBlank reports whether img is effectively one flat color,
// which is what a screenshot of a client-rendered page looks like when the
// capture beat the page's JavaScript to the paint.
func previewImageIsBlank(img image.Image) bool {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < previewMinImageDimension || height < previewMinImageDimension {
		return true
	}

	stepX := max(1, width/previewBlankSampleGrid)
	stepY := max(1, height/previewBlankSampleGrid)
	referenceR, referenceG, referenceB, _ := img.At(bounds.Min.X+width/2, bounds.Min.Y+height/2).RGBA()

	samples, matching := 0, 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			samples++
			if channelsMatch(r, referenceR) && channelsMatch(g, referenceG) && channelsMatch(b, referenceB) {
				matching++
			}
		}
	}
	if samples == 0 {
		return true
	}
	return float64(matching)/float64(samples) >= previewBlankUniformRatio
}

// channelsMatch reports whether two 16-bit color channels are within
// previewBlankChannelTolerance of each other on an 8-bit scale.
func channelsMatch(a, b uint32) bool {
	high, low := a>>8, b>>8
	if high < low {
		high, low = low, high
	}
	return high-low <= previewBlankChannelTolerance
}

// previewWarmer screens and captures preview targets in the background, so
// that by the time a visitor loads the page the server already knows which
// origins are healthy and has their screenshots cached.
type previewWarmer struct {
	state    *previewState
	cacheDir string
	client   *http.Client
	probe    func(*http.Client, string) error
	fetch    func(*http.Client, string) ([]byte, error)
}

func newPreviewWarmer(state *previewState, cacheDir string, client *http.Client) *previewWarmer {
	return &previewWarmer{
		state:    state,
		cacheDir: cacheDir,
		client:   client,
		probe:    probePreviewTarget,
		fetch:    fetchPreviewImage,
	}
}

// warmAll screens and captures every indexed target that isn't already
// backed by a fresh cached image, a few at a time. Targets whose last
// verdict was "unavailable" are always retried, which is how a site that
// comes back up recovers its preview without a restart.
func (warmer *previewWarmer) warmAll(ctx context.Context) {
	targets := warmer.state.targets()
	if len(targets) == 0 {
		return
	}
	log.Printf("preview warm: screening %d target(s)", len(targets))
	errorCaptureReference.ensure(warmer.client)

	slots := make(chan struct{}, previewWarmConcurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		slots <- struct{}{}
		go func(target string) {
			defer wg.Done()
			defer func() { <-slots }()
			if ctx.Err() != nil {
				return
			}
			warmer.warmOne(target)
		}(target)
	}
	wg.Wait()
	log.Print("preview warm: pass complete")
}

// warmOne captures target unless a fresh capture is already cached and the
// target's last verdict was healthy.
func (warmer *previewWarmer) warmOne(target string) {
	status, screened := warmer.state.availability(target)
	if screened && status.ok {
		if _, cached := warmer.state.cachedPreview(warmer.cacheDir, target); cached {
			return
		}
	}
	if !warmer.state.beginCapture(target) {
		return
	}
	defer warmer.state.endCapture(target)
	if _, err := warmer.state.capturePreview(warmer.client, warmer.probe, warmer.fetch, warmer.cacheDir, target); err == nil {
		log.Printf("preview warm: captured %s", target)
	}
}

// runPreviewWarmer warms every target once, then again on each tick of
// interval until ctx is canceled.
func runPreviewWarmer(ctx context.Context, warmer *previewWarmer, interval time.Duration) {
	warmer.warmAll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			warmer.warmAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// getFreshPreviewFromMemory returns the cached preview image for key from
// previewCache if present and not yet expired, evicting it if it has
// expired.
func getFreshPreviewFromMemory(key string) ([]byte, bool) {
	return defaultPreviewState.getFreshFromMemory(key)
}

func (state *previewState) getFreshFromMemory(key string) ([]byte, bool) {
	now := time.Now()
	state.cacheMu.RLock()
	item, ok := state.cacheItems[key]
	state.cacheMu.RUnlock()
	if !ok || now.After(item.expiresAt) {
		if ok {
			log.Printf("preview cache: evicting expired memory entry %s", key)
			state.cacheMu.Lock()
			delete(state.cacheItems, key)
			state.cacheMu.Unlock()
		}
		return nil, false
	}
	return item.data, true
}

// storePreviewInMemory caches data in previewCache under key, setting a
// fresh expiration previewTTL from now. The stored bytes are copied so
// callers may reuse their buffer.
func storePreviewInMemory(key string, data []byte) {
	defaultPreviewState.storeInMemory(key, data)
}

func (state *previewState) storeInMemory(key string, data []byte) {
	state.cacheMu.Lock()
	state.cacheItems[key] = previewCacheEntry{
		data:      append([]byte(nil), data...),
		expiresAt: time.Now().Add(previewTTL),
	}
	state.cacheMu.Unlock()
}

// getFreshPreviewFromDisk reads a cached preview image from path if it
// exists and is younger than ttl, removing it if it has expired.
func getFreshPreviewFromDisk(path string, ttl time.Duration) ([]byte, time.Time, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, time.Time{}, false
	}
	if time.Since(info.ModTime()) > ttl {
		log.Printf("preview cache: removing expired disk entry %s", path)
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
// whose modification time is older than ttl. It is run periodically to keep
// the on-disk cache bounded.
func purgeExpiredPreviewFiles(cacheDir string, ttl time.Duration) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}

	log.Printf("preview cache: purging entries older than %s from %s (%d file(s) on disk)", ttl, cacheDir, len(entries))

	now := time.Now()
	removed := 0
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
			removed++
		}
	}
	log.Printf("preview cache: purge complete, removed %d expired file(s) from %s", removed, cacheDir)
}

// invalidatePreviewCache drops every cached preview, on disk and in memory.
// It runs at startup: a restart is the one moment we can be sure someone
// wants the site's screenshots reconsidered, and it means a capture taken
// while an origin was broken never outlives the process that took it.
func invalidatePreviewCache(cacheDir string, state *previewState) {
	if state != nil {
		state.cacheMu.Lock()
		state.cacheItems = make(map[string]previewCacheEntry)
		state.cacheMu.Unlock()
		state.statusMu.Lock()
		state.statuses = make(map[string]previewAvailability)
		state.statusMu.Unlock()
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(cacheDir, entry.Name())); err == nil {
			removed++
		}
	}
	log.Printf("preview cache: invalidated on startup, removed %d file(s) from %s", removed, cacheDir)
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

// servePreviewPlaceholder redirects an unusable preview to the Artisan
// Studios lockup rather than leaving a broken image — or, worse, a captured
// error page — on the card. The redirect is deliberately uncacheable so the
// next load reconsiders the origin.
func servePreviewPlaceholder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Sec-CH-Prefers-Color-Scheme")
	http.Redirect(w, r, previewPlaceholderSrc(requestPrefersDark(r)), http.StatusTemporaryRedirect)
}

// previewPlaceholderSrc picks the lockup whose ink reads against the
// visitor's background.
func previewPlaceholderSrc(prefersDark bool) string {
	if prefersDark {
		return previewPlaceholderDarkSrc
	}
	return previewPlaceholderLightSrc
}

// requestPrefersDark reads the Sec-CH-Prefers-Color-Scheme client hint,
// requested by htmlPreviewRewriteHandler. Browsers that don't send it fall
// through to the light-theme lockup, matching the site's default theme.
func requestPrefersDark(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Sec-CH-Prefers-Color-Scheme")), "dark")
}
