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
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// main parses command-line flags, wires up all HTTP routes, starts the
// background static-file watcher and preview cache janitor, and blocks
// serving HTTP until the process exits.
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

// run parses configuration, constructs the server, starts background work,
// and serves until ctx is canceled. Keeping lifecycle work out of main makes
// startup and shutdown testable without special process hooks.
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("artisan-webserver", flag.ContinueOnError)
	port := fs.Int("port", 8082, "port to listen on")
	previewCacheDirFlag := fs.String("preview-cache-dir", defaultPreviewCacheDir, "directory for persisted server-side preview cache")
	envPathFlag := fs.String("env-path", ".env", "path to a .env file to load into the process environment")
	websiteFilesFlag := fs.String("website-files", "", "path to the static site directory; overrides WEBSITE_FILES from the env file/environment (default \"static\")")
	simulatedError := fs.String("sim-error", "", "dev-only: force /api/contact into a simulated failure mode (500, 503, 429, timeout, drop) to test frontend error handling")
	if err := fs.Parse(args); err != nil {
		return err
	}

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
	var currentSiteBuildHash atomic.Value
	currentSiteBuildHash.Store(initialBuildHash)
	log.Printf("site build hash: %s (from %s)", initialBuildHash, staticDir)
	previews := newPreviewState()
	previews.rebuildSourceIndex(staticDir, initialBuildHash)
	currentBuildHash := func() string {
		if v := currentSiteBuildHash.Load(); v != nil {
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
	hub := newReloadHub()
	imageCache := newWebPConversionCache()
	handler := newServerHandler(serverConfig{
		staticDir:       staticDir,
		previewCacheDir: previewCacheDir,
		simError:        *simulatedError,
		buildHash:       currentBuildHash,
		hub:             hub,
		previewState:    previews,
		imageCache:      imageCache,
	}, serverDependencies{})
	addr := fmt.Sprintf("0.0.0.0:%d", *port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	// Watch for changes
	go watchFilesContext(ctx, staticDir, func() {
		nextHash := computeSiteBuildHash(staticDir)
		currentSiteBuildHash.Store(nextHash)
		log.Printf("site build hash updated: %s", nextHash)
		previews.rebuildSourceIndex(staticDir, nextHash)
	}, hub)

	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				purgeExpiredPreviewFiles(previewCacheDir, previewTTL)
				pruneExpiredImageCache(imageCache, imageCacheTTL)
			case <-ctx.Done():
				return
			}
		}
	}()

	server := &http.Server{Handler: handler}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("server shutdown: %v", err)
		}
	}()

	log.Printf("Starting server on %s", listener.Addr())
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
