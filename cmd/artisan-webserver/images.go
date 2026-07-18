package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chai2010/webp"
	"golang.org/x/image/draw"
)

const (
	// maxImageWidth and maxImageHeight are the default dimensions on-the-fly
	// WebP conversions under /imgs/ are downscaled to when a request doesn't
	// override them via the w/h query params (see parseImageRequestOptions).
	maxImageWidth  = 512
	maxImageHeight = 256
	// webpQuality is the default lossy encoding quality used for /imgs/
	// conversions when a request doesn't supply a q query param.
	webpQuality = 60
	// minWebPQuality and maxWebPQuality clamp the q query param.
	minWebPQuality = 30
	maxWebPQuality = 95
	// imageCacheTTL bounds how long a converted /imgs/ variant may sit idle
	// in memory before it's evicted. Every cache hit refreshes an entry's
	// lastAccessed time, so actively-requested images stay cached
	// indefinitely; only variants nobody has asked for in imageCacheTTL age
	// out. This keeps long-running deployments (months of uptime) from
	// accumulating an unbounded number of cached size/quality variants.
	imageCacheTTL = 24 * time.Hour
)

// imageRequestOptions carries the resolved per-request resize/quality
// settings for one /imgs/ conversion, derived from the w/h/q query params.
type imageRequestOptions struct {
	maxWidth  int
	maxHeight int
	quality   float32
}

// cachedWebP holds a previously converted WebP image plus the source file's
// modification time (used to detect when the cached copy is stale) and the
// last time it was served (used to evict idle entries, see imageCacheTTL).
type cachedWebP struct {
	data         []byte
	modTime      time.Time
	lastAccessed time.Time
}

type webpConversionCache struct {
	sync.RWMutex
	items map[string]cachedWebP
}

func newWebPConversionCache() *webpConversionCache {
	return &webpConversionCache{items: make(map[string]cachedWebP)}
}

// webpCache backs the legacy handler constructor. Constructed servers use an
// instance-owned cache so tests and multiple servers remain isolated.
var webpCache = newWebPConversionCache()

// pruneExpiredImageCache removes cached WebP variants that haven't been
// served in ttl, reclaiming memory from images nobody is requesting anymore
// (e.g. removed from the site, or a superseded w/h/q variant). Actively
// requested entries are untouched because a cache hit refreshes
// lastAccessed. Run periodically (see run() in main.go) rather than relying
// solely on the lazy check in optimizedImageHandlerWithCache, since that
// only evicts an entry when something asks for it again.
func pruneExpiredImageCache(cache *webpConversionCache, ttl time.Duration) {
	cache.Lock()
	defer cache.Unlock()
	now := time.Now()
	removed := 0
	for key, entry := range cache.items {
		if now.Sub(entry.lastAccessed) > ttl {
			delete(cache.items, key)
			removed++
		}
	}
	if removed > 0 {
		log.Printf("image cache: pruned %d expired entr(ies), %d remaining", removed, len(cache.items))
	}
}

// optimizedImageHandler returns an http.HandlerFunc that serves images under
// staticDir as WebP when the requesting client advertises WebP support via
// its Accept header, converting and caching them on first request. Requests
// that are not eligible (wrong method, unsupported client, unsupported
// extension, missing file) fall back to the provided fallback handler
// (typically the static file server).
func optimizedImageHandler(staticDir string, fallback http.Handler) http.HandlerFunc {
	return optimizedImageHandlerWithCache(staticDir, fallback, webpCache)
}

func optimizedImageHandlerWithCache(staticDir string, fallback http.Handler, cache *webpConversionCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			fallback.ServeHTTP(w, r)
			return
		}

		if !supportsWebP(r) {
			fallback.ServeHTTP(w, r)
			return
		}

		relPath := strings.TrimPrefix(r.URL.Path, "/")
		cleanRel := filepath.Clean(relPath)
		if cleanRel == "." || strings.HasPrefix(cleanRel, "..") {
			fallback.ServeHTTP(w, r)
			return
		}

		fullPath := filepath.Join(staticDir, cleanRel)
		ext := strings.ToLower(filepath.Ext(fullPath))

		switch ext {
		case ".jpg", ".jpeg", ".png", ".gif", ".webp":
			// eligible for conversion
		default:
			fallback.ServeHTTP(w, r)
			return
		}

		info, err := os.Stat(fullPath)
		if err != nil || info.IsDir() {
			fallback.ServeHTTP(w, r)
			return
		}

		opts := parseImageRequestOptions(r)
		key := fmt.Sprintf("%s|w=%d|h=%d|q=%0.1f", fullPath, opts.maxWidth, opts.maxHeight, opts.quality)

		cache.RLock()
		cached, ok := cache.items[key]
		cache.RUnlock()

		if ok && info.ModTime().Equal(cached.modTime) && time.Since(cached.lastAccessed) <= imageCacheTTL {
			log.Printf("image cache: hit for %s", fullPath)
			cache.Lock()
			cached.lastAccessed = time.Now()
			cache.items[key] = cached
			cache.Unlock()
			serveWebP(w, r, fullPath, cached.modTime, cached.data)
			return
		}

		log.Printf("image cache: miss for %s, converting to webp", fullPath)

		file, err := os.Open(fullPath)
		if err != nil {
			fallback.ServeHTTP(w, r)
			return
		}
		defer file.Close()

		dataBytes, err := io.ReadAll(file)
		if err != nil {
			fallback.ServeHTTP(w, r)
			return
		}

		var img image.Image
		if ext == ".webp" {
			img, err = webp.Decode(bytes.NewReader(dataBytes))
		} else {
			img, _, err = image.Decode(bytes.NewReader(dataBytes))
		}
		if err != nil {
			fallback.ServeHTTP(w, r)
			return
		}

		processed := resizeIfNeeded(img, opts.maxWidth, opts.maxHeight)

		var buf bytes.Buffer
		if err := webp.Encode(&buf, processed, &webp.Options{Quality: opts.quality}); err != nil {
			fallback.ServeHTTP(w, r)
			return
		}

		data := buf.Bytes()
		cache.Lock()
		cache.items[key] = cachedWebP{data: data, modTime: info.ModTime(), lastAccessed: time.Now()}
		cache.Unlock()
		log.Printf("image cache: stored %s (%d bytes)", fullPath, len(data))

		serveWebP(w, r, fullPath, info.ModTime(), data)
	}
}

// serveWebP writes data as an image/webp response with a long-lived cache
// header, honoring conditional GET semantics via http.ServeContent.
func serveWebP(w http.ResponseWriter, r *http.Request, path string, modTime time.Time, data []byte) {
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", "public, max-age=31536000")
	w.Header().Add("Vary", "Accept")
	http.ServeContent(w, r, filepath.Base(path), modTime, bytes.NewReader(data))
}

// supportsWebP reports whether the request's Accept header indicates the
// client can render image/webp.
func supportsWebP(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if accept == "" {
		return false
	}
	return strings.Contains(accept, "image/webp")
}

// parseImageRequestOptions resolves the w/h/q query params on an /imgs/
// request into concrete resize/quality settings.
//
//   - If neither w nor h is given, both default to maxImageWidth/maxImageHeight.
//   - If exactly one of w/h is given, the other is left unconstrained (-1)
//     rather than defaulted, so e.g. "?w=1200" alone scales to exactly
//     1200px wide at the source aspect ratio instead of being boxed into
//     the default bounds.
//   - q defaults to webpQuality and is clamped to [minWebPQuality, maxWebPQuality].
//
// Invalid or non-positive values are treated the same as absent ones.
func parseImageRequestOptions(r *http.Request) imageRequestOptions {
	maxWidth := parsePositiveInt(r.URL.Query().Get("w"))
	maxHeight := parsePositiveInt(r.URL.Query().Get("h"))
	quality := parsePositiveInt(r.URL.Query().Get("q"))

	if maxWidth == 0 && maxHeight == 0 {
		maxWidth = maxImageWidth
		maxHeight = maxImageHeight
	} else {
		if maxWidth == 0 {
			maxWidth = -1
		}
		if maxHeight == 0 {
			maxHeight = -1
		}
	}

	q := webpQuality
	if quality > 0 {
		switch {
		case quality < minWebPQuality:
			q = minWebPQuality
		case quality > maxWebPQuality:
			q = maxWebPQuality
		default:
			q = quality
		}
	}

	return imageRequestOptions{maxWidth: maxWidth, maxHeight: maxHeight, quality: float32(q)}
}

// parsePositiveInt parses raw as a positive int, returning 0 for anything
// empty, malformed, or non-positive so callers can treat it as "unset".
func parsePositiveInt(raw string) int {
	if raw == "" {
		return 0
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

// resizeIfNeeded downscales src to fit within maxWidth x maxHeight, leaving
// it untouched if it already fits. A maxWidth or maxHeight below 1 (the
// sentinel parseImageRequestOptions produces for an unconstrained axis) is
// resolved to src's own dimension on that axis, i.e. no constraint.
func resizeIfNeeded(src image.Image, maxWidth, maxHeight int) image.Image {
	bounds := src.Bounds()
	if maxWidth < 1 {
		maxWidth = bounds.Dx()
	}
	if maxHeight < 1 {
		maxHeight = bounds.Dy()
	}
	return resizeToFit(src, maxWidth, maxHeight)
}

// resizeToFit downscales src, preserving aspect ratio, so that it fits
// within maxWidth x maxHeight. If src is already within those bounds it is
// returned unchanged; otherwise a new RGBA image is produced using
// Catmull-Rom resampling.
func resizeToFit(src image.Image, maxWidth, maxHeight int) image.Image {
	bounds := src.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	if width <= maxWidth && height <= maxHeight {
		return src
	}

	scale := math.Min(float64(maxWidth)/float64(width), float64(maxHeight)/float64(height))
	newWidth := int(math.Round(float64(width) * scale))
	newHeight := int(math.Round(float64(height) * scale))

	if newWidth < 1 {
		newWidth = 1
	}
	if newHeight < 1 {
		newHeight = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, bounds, draw.Over, nil)
	return dst
}
