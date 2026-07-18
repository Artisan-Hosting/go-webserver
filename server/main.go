// Command server runs the Artisan Studios static web server. Alongside
// serving static files, it optimizes images to WebP on the fly, rewrites
// and caches server-side link-preview images, handles the site's contact
// form, and provides a live-reload endpoint for local development.
//
// The implementation is split across files by responsibility:
//
//   - main.go            - flag parsing and server/route wiring
//   - env.go              - .env file loading
//   - captcha.go          - Cap captcha verification for the contact form
//   - mail.go             - outbound mail relay client
//   - contact.go          - the /api/contact form handler
//   - email_templates.go  - HTML/plain-text email bodies
//   - images.go           - on-the-fly WebP conversion for /imgs/
//   - preview.go          - server-side link-preview fetching/caching
//   - watch.go            - filesystem watching and live-reload SSE
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// main parses command-line flags, wires up all HTTP routes, starts the
// background static-file watcher and preview cache janitor, and blocks
// serving HTTP until the process exits.
func main() {
	port := flag.Int("port", 8082, "port to listen on")
	previewCacheDirFlag := flag.String("preview-cache-dir", defaultPreviewCacheDir, "directory for persisted server-side preview cache")
	envPathFlag := flag.String("env-path", "server/.env", "path to a .env file to load into the process environment")
	websiteFilesFlag := flag.String("website-files", "", "path to the static site directory; overrides WEBSITE_FILES from the env file/environment (default \"static\")")
	flag.Parse()

	loadDotEnv(*envPathFlag)

	staticDir := "static"
	if envDir := strings.TrimSpace(os.Getenv("WEBSITE_FILES")); envDir != "" {
		staticDir = envDir
	}
	if *websiteFilesFlag != "" {
		// CLI flag always wins over the env file/environment, even if both are set.
		staticDir = *websiteFilesFlag
	}
	staticDir = filepath.Clean(staticDir)
	previewCacheDir := filepath.Clean(*previewCacheDirFlag)
	initialBuildHash := computeSiteBuildHash(staticDir)
	siteBuildHash.Store(initialBuildHash)
	rebuildPreviewSourceIndex(staticDir, initialBuildHash)
	currentBuildHash := func() string {
		if v := siteBuildHash.Load(); v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return "dev"
	}

	if err := os.MkdirAll(previewCacheDir, 0o755); err != nil {
		log.Printf("failed to create preview cache dir %s: %v", previewCacheDir, err)
	}
	purgeExpiredPreviewFiles(previewCacheDir, previewTTL)

	// Serve static files
	fs := http.FileServer(http.Dir(staticDir))
	http.HandleFunc("/imgs/", optimizedImageHandler(staticDir, fs))
	http.HandleFunc("/__preview/", previewImageHandler(previewCacheDir, previewSourceByHash))
	http.Handle("/", htmlPreviewRewriteHandler(staticDir, fs, currentBuildHash))

	// contact endpoint
	http.HandleFunc("/api/contact", contactHandler)
	http.HandleFunc("/api/captcha-config", captchaConfigHandler)

	// SSE endpoint to notify changes
	http.HandleFunc("/reload", reloadHandler)

	// Watch for changes
	go watchFiles(staticDir, func() {
		nextHash := computeSiteBuildHash(staticDir)
		siteBuildHash.Store(nextHash)
		rebuildPreviewSourceIndex(staticDir, nextHash)
	})

	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			purgeExpiredPreviewFiles(previewCacheDir, previewTTL)
		}
	}()

	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	log.Printf("Starting server on :%d", *port)
	log.Fatal(http.ListenAndServe(addr, nil))
}
