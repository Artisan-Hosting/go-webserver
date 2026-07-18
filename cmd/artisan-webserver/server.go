package main

import (
	"net/http"
	"time"
)

type serverConfig struct {
	staticDir       string
	previewCacheDir string
	simError        string
	buildHash       func() string
	hub             *reloadHub
	imageCache      *webpConversionCache
	previewState    *previewState
}

type serverDependencies struct {
	verifyCaptcha func(endpoint, secret, token string) (bool, error)
	deliverMail   func(EmailPayload) error
	previewClient *http.Client
	fetchPreview  func(*http.Client, string) ([]byte, error)
	now           func() time.Time
}

// newServerHandler wires the complete HTTP surface on an isolated mux. Tests
// can replace external dependencies while production receives the defaults.
func newServerHandler(cfg serverConfig, deps serverDependencies) http.Handler {
	if cfg.buildHash == nil {
		cfg.buildHash = func() string { return "dev" }
	}
	if cfg.hub == nil {
		cfg.hub = newReloadHub()
	}
	if cfg.imageCache == nil {
		cfg.imageCache = newWebPConversionCache()
	}
	if cfg.previewState == nil {
		cfg.previewState = newPreviewState()
	}
	if deps.verifyCaptcha == nil {
		deps.verifyCaptcha = verifyCaptcha
	}
	if deps.deliverMail == nil {
		deps.deliverMail = sendMail
	}
	if deps.previewClient == nil {
		deps.previewClient = &http.Client{Timeout: 12 * time.Second}
	}
	if deps.fetchPreview == nil {
		deps.fetchPreview = fetchPreviewImage
	}
	if deps.now == nil {
		deps.now = time.Now
	}

	staticFiles := http.FileServer(http.Dir(cfg.staticDir))
	mux := http.NewServeMux()
	mux.HandleFunc("/imgs/", optimizedImageHandlerWithCache(cfg.staticDir, staticFiles, cfg.imageCache))
	mux.HandleFunc("/__preview/", previewImageHandlerWithStateDependencies(cfg.previewCacheDir, cfg.previewState.sourceByHash, deps.previewClient, deps.fetchPreview, deps.now, cfg.previewState))
	mux.Handle("/", htmlPreviewRewriteHandler(cfg.staticDir, staticFiles, cfg.buildHash))
	mux.HandleFunc("/api/contact", contactHandlerWithDependencies(cfg.simError, deps.verifyCaptcha, deps.deliverMail))
	mux.HandleFunc("/api/captcha-config", captchaConfigHandler)
	mux.HandleFunc("/reload", reloadHandlerWithHub(cfg.hub))
	return mux
}
