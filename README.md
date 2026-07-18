# Artisan Studios Web Server

A single Go binary that serves a static marketing/portfolio site, plus a
handful of small dynamic features layered on top:

- **Static file serving** with automatic on-the-fly WebP conversion for
  images requested under `/imgs/`.
- **Server-side link-preview images** — HTML pages under the static site
  get their `<img>` previews fetched, cached, and rewritten server-side.
- **Contact form** (`/api/contact`) that emails both the submitter and the
  site owner, with optional [Cap](https://trycap.dev) captcha verification.
- **Themeable transactional email** — see [`docs/MAIL_THEMING.md`](docs/MAIL_THEMING.md).
- **Live reload** during local development via a `/reload` Server-Sent-Events
  endpoint that fires whenever a static file changes.

## Directory structure

```
.
├── docs/
│   └── MAIL_THEMING.md   # transactional email layout/theming reference
├── makefile              # build/run entry points
├── main.go               # flag parsing, route wiring, startup
├── env.go                # .env file loading
├── captcha.go            # Cap captcha verification for the contact form
├── mail.go               # outbound mail relay client + theming engine
├── mailtheme.go          # fixed HTML email skeleton (hermes.Theme)
├── contact.go            # POST /api/contact handler
├── email_templates.go    # HTML/plain-text email bodies (data only)
├── images.go             # on-the-fly WebP conversion for /imgs/
├── preview.go            # server-side link-preview fetching/caching
├── watch.go              # filesystem watching + live-reload SSE
├── theme/
│   └── default.css       # default transactional-email theme
├── go.mod / go.sum
└── bin                   # build output (gitignored, not committed)
```

`static/` (the site content served at `/`) and `.env` (per-deployment
secrets/config) are **not part of this repo** — they're provided by whatever
site deploys this server. See [Deployment](#deployment) below.

## Requirements

- Go 1.24.2+ (see `go.mod`; CI/dev machines should have at least the
  `go1.24.7` toolchain it pins).

## Quick start

```sh
make build   # go build -o ./bin
make run     # build, then run ./bin from the repo root
```

The server needs to run with its working directory at the repo root (or
wherever `static/` and this repo live side by side for your deployment) —
all relative paths (`static`, `.env`, `theme/default.css`, etc.) are
resolved from there, unless overridden by the flags below.

## CLI flags

All flags are optional; defaults preserve the server's original behavior.

| Flag | Default | Effect |
|---|---|---|
| `-port` | `8082` | Port to listen on. |
| `-env-path` | `.env` | Path to the `.env` file to load into the process environment at startup. |
| `-website-files` | *(unset)* | Path to the static site directory. Overrides `WEBSITE_FILES` from the env file/environment. Falls back to `WEBSITE_FILES`, then to `"static"`, if unset. |
| `-preview-cache-dir` | `$TMPDIR/artisan-webserver/previews` | Directory for persisted server-side link-preview images. |
| `-sim-error` | *(unset)* | Dev-only: force `/api/contact` into a simulated failure mode (`500`, `503`, `429`, `timeout`, `drop`) to test frontend error handling without breaking anything real. See [`docs/CAPTCHA_FRONTEND.md`](docs/CAPTCHA_FRONTEND.md#simulating-failures-for-ui-testing--sim-error-). |

Example:

```sh
./bin -port 4000 -website-files /var/www/example/static -env-path /etc/example/.env
```

## Environment variables

Read from the process environment — normally via the `.env` file at
`-env-path` (loaded by `loadDotEnv()`, see `env.go`), or the real
process environment, which always wins over the `.env` file. `WEBSITE_FILES`
additionally sits underneath the `-website-files` CLI flag, which wins over
both.

| Variable | Default | Effect |
|---|---|---|
| `WEBSITE_FILES` | `static` | Static site directory. Overridable per-request-for-debugging via `-website-files`. |
| `CAPTCHA_API_ENDPOINT` | *(unset)* | Self-hosted [Cap](https://trycap.dev) instance site path, e.g. `https://cap.example.com/<site-key>/`. Leaving this or `CAPTCHA_SECRET_KEY` unset disables captcha enforcement on the contact form. |
| `CAPTCHA_SECRET_KEY` | *(unset)* | Secret key matching the Cap site key above. |
| `CONTACT_OWNER_EMAIL` | `info@artisanhosting.net` | Where the contact form's owner-notification email is sent. |
| `MAIL_THEME_CSS` | `theme/default.css` | Path to the transactional-email theme CSS file. |
| `MAIL_PRODUCT_NAME` | `Artisan Studios` | Shown in email masthead, footer, signature. |
| `MAIL_PRODUCT_LINK` | `https://www.artisanhosting.net` | Email masthead/footer link target. |
| `MAIL_PRODUCT_LOGO` | *(unset)* | Masthead logo image URL; falls back to product name as text. |

See [`docs/MAIL_THEMING.md`](docs/MAIL_THEMING.md) for the full theming
system and how to add a per-site theme.

## HTTP endpoints

| Method & path | Handler | Purpose |
|---|---|---|
| `GET /` | `htmlPreviewRewriteHandler` | Serves the static site; rewrites `<img>` preview tags in HTML responses. |
| `GET /imgs/*` | `optimizedImageHandler` | Serves images, converting to WebP on the fly when the client supports it. |
| `GET /__preview/*` | `previewImageHandler` | Serves cached server-side link-preview images. |
| `POST /api/contact` | `contactHandler` | Contact form submission: validates captcha (if enabled), emails submitter + owner. |
| `GET /api/captcha-config` | `captchaConfigHandler` | Public captcha config (enabled flag + endpoint) for the frontend widget. |
| `GET /reload` | `reloadHandler` | Server-Sent-Events stream that fires on static file changes, for live reload during local dev. |

## Adding this to a project

This repo is the shared server engine (module path
`github.com/Artisan-Hosting/go-webserver`). There are two ways to pull it
into a site:

**New projects — `go install`, no source checkout.** Since `-website-files`
and `-env-path` let a single installed binary point at any site's
`static/`/`.env`, you generally don't need to vendor the source at all:

```sh
go install github.com/Artisan-Hosting/go-webserver@latest
```

This installs a binary named `go-webserver` into `$(go env GOPATH)/bin`.
Run it from the site's root (so `static`/`.env` resolve normally), or pass
`-website-files`/`-env-path` explicitly to point it elsewhere. Pin a
released tag (`@v0.1.0`, etc.) instead of `@latest` once this repo starts
tagging releases, for reproducible deploys.

**Existing projects that vendor the source** — add this repo as a git
submodule (conventionally mounted at `server/`), then build with
`make build`.

## Deployment

This repo is the shared server engine. A deployed site typically looks like:

```
/var/www/<site>/
├── server/       # this repo, checked out here
├── static/       # the site's own static content
└── server/.env   # the site's own secrets/config (see Environment variables above)
```

Example systemd unit:

```ini
[Unit]
Description=<site> Go web service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=www-data
Group=www-data
WorkingDirectory=/var/www/<site>
ExecStartPre=/usr/bin/make -C /var/www/<site> build
ExecStart=/var/www/<site>/server/bin -port 4000
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
```

Use `-website-files` / `-env-path` instead if a deployment needs its static
content or `.env` file to live somewhere other than the conventional
`static/` / `server/.env` locations (e.g. for debugging against a different
content directory without touching the environment).

## Testing

```sh
go test ./...
```
