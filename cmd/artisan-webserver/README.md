# Server source guide

This directory is one Go `package main`; production files and their matching
`*_test.go` files stay together so tests can exercise internal behavior without
exporting implementation details.

## Runtime and wiring

- `main.go` parses flags and owns process startup and shutdown.
- `server.go` constructs the isolated HTTP route mux and injects dependencies.
- `env.go` loads deployment configuration from `.env`.

## HTTP features

- `contact.go` and `captcha.go` implement the contact-form API.
- `images.go` handles on-demand WebP conversion.
- `preview.go` rewrites, fetches, and caches server-side previews.
- `watch.go` implements filesystem watching and live-reload SSE.

## Email system

- `mail.go` owns relay delivery and Hermes configuration.
- `email_contract.go` defines the site-override contract.
- `email_templates.go` contains the default site copy.
- `mailtheme.go` contains the shared HTML and plain-text layout.

Tests are named after the behavior they cover. `server_process_test.go` is the
only integration-tagged test; the remaining tests run in the normal fast suite.
