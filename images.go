package main

import (
	"bytes"
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
	"strings"
	"sync"
	"time"

	"github.com/chai2010/webp"
	"golang.org/x/image/draw"
)

const (
	// maxImageWidth and maxImageHeight bound the dimensions that on-the-fly
	// WebP conversions under /imgs/ are downscaled to.
	maxImageWidth  = 512
	maxImageHeight = 256
	// webpQuality is the lossy encoding quality used for /imgs/ conversions.
	webpQuality = 60
)

// cachedWebP holds a previously converted WebP image plus the source file's
// modification time, used to detect when the cached copy is stale.
type cachedWebP struct {
	data    []byte
	modTime time.Time
}

// webpCache memoizes converted WebP images for /imgs/ requests, keyed by the
// absolute path of the source file on disk.
var webpCache = struct {
	sync.RWMutex
	items map[string]cachedWebP
}{items: make(map[string]cachedWebP)}

// optimizedImageHandler returns an http.HandlerFunc that serves images under
// staticDir as WebP when the requesting client advertises WebP support via
// its Accept header, converting and caching them on first request. Requests
// that are not eligible (wrong method, unsupported client, unsupported
// extension, missing file) fall back to the provided fallback handler
// (typically the static file server).
func optimizedImageHandler(staticDir string, fallback http.Handler) http.HandlerFunc {
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

		key := fullPath

		webpCache.RLock()
		cached, ok := webpCache.items[key]
		webpCache.RUnlock()

		if ok && info.ModTime().Equal(cached.modTime) {
			log.Printf("image cache: hit for %s", fullPath)
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

		processed := resizeIfNeeded(img)

		var buf bytes.Buffer
		if err := webp.Encode(&buf, processed, &webp.Options{Quality: webpQuality}); err != nil {
			fallback.ServeHTTP(w, r)
			return
		}

		data := buf.Bytes()
		webpCache.Lock()
		webpCache.items[key] = cachedWebP{data: data, modTime: info.ModTime()}
		webpCache.Unlock()
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

// resizeIfNeeded downscales src to fit within the standard /imgs/ bounds
// (maxImageWidth x maxImageHeight), leaving it untouched if it already fits.
func resizeIfNeeded(src image.Image) image.Image {
	return resizeToFit(src, maxImageWidth, maxImageHeight)
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
