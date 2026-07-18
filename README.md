# Artisan Studios Web Server

A single Go binary that serves a static marketing/portfolio site, plus a
handful of small dynamic features layered on top:

- **Static file serving** with automatic on-the-fly WebP conversion for
  images requested under `/imgs/`, optionally resized/re-encoded per request
  via `w`/`h`/`q` query params (see the endpoint table below).
- **Server-side link-preview images** — HTML pages under the static site
  get their `<img>` previews fetched, cached, and rewritten server-side.
- **Contact form** (`/api/contact`) that emails both the submitter and the
  site owner, with optional [Cap](https://trycap.dev) captcha verification.
  See [`docs/CAPTCHA_FRONTEND.md`](docs/CAPTCHA_FRONTEND.md) for how to build
  the frontend side of this against the endpoints below.
- **Themeable transactional email** — see [`docs/MAIL_THEMING.md`](docs/MAIL_THEMING.md).
- **Live reload** during local development via a `/reload` Server-Sent-Events
  endpoint that fires whenever a static file changes.

## Directory structure

```
.
├── cmd/
│   └── artisan-webserver/  # server source and co-located Go tests
├── docs/
│   ├── MAIL_THEMING.md      # transactional email layout/theming reference
│   └── CAPTCHA_FRONTEND.md  # frontend contact-form + Cap captcha integration reference
├── theme/
│   ├── default.css          # default transactional-email theme
│   └── testdata/            # alternate theme fixture used by tests
├── makefile                 # build, run, and test entry points
├── go.mod / go.sum
└── bin                   # build output (gitignored, not committed)
```

All Go source stays together under [`cmd/artisan-webserver/`](cmd/artisan-webserver/README.md)
because it is one `package main`. This keeps the submodule root easy to scan
without introducing another `server/` directory or changing the site override
contract. The source directory's README groups files by responsibility.

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

### `/imgs/*` resize/quality query params

Requests may override the default WebP conversion bounds with query params;
converted variants are cached separately per resolved option set:

| Param | Default | Effect |
|---|---|---|
| `w` | `512` | Max output width in pixels. If set without `h`, height is left unconstrained (scales to exactly `w` wide at the source aspect ratio). |
| `h` | `256` | Max output height in pixels. If set without `w`, width is left unconstrained. |
| `q` | `60` | WebP encoding quality, clamped to `[30, 95]`. |

Images are only ever downscaled, never upscaled — a source already within
the resolved bounds is served as-is (after WebP conversion). Example:
`/imgs/servers.jpg?w=1200&h=1600&q=80`.

Converted variants are cached in memory per `(file, w, h, q)` combination.
Each cache hit refreshes that entry's last-served time, so actively
requested images stay cached indefinitely; an entry nobody has requested in
24h (`imageCacheTTL`) is treated as stale on its next request and
reconverted. A background sweep also runs every 24h to actively evict idle
entries, so long-running deployments don't accumulate an unbounded number of
cached size/quality variants over months of uptime.

## Adding this to a project

This repo is the shared server engine (module path
`github.com/Artisan-Hosting/go-webserver`). Add it to each site as a git
submodule, conventionally mounted at `server/`, then build that submodule.

Sites that need custom contact email subjects, copy, links, or CTAs should
keep the shared submodule clean and place overrides in the parent repo:

```text
client-site/
├── server/                  # this repo as a submodule
├── server_overrides/
│   └── email_templates.go   # package main, site-specific Hermes templates
└── static/
```

Build with:

```sh
make -C server build SITE_OVERRIDES=../server_overrides
```

The build copies this repo into `server/.build/site-server`, overlays the
override directory, and compiles from that disposable tree. If
`server_overrides/email_templates.go` exists, it replaces the default
`email_templates.go` for that build without dirtying the submodule.

See [`docs/MAIL_THEMING.md`](docs/MAIL_THEMING.md#customize-contact-emails-per-site)
for the `ContactEmailTemplates` contract and an example override.

## Deployment

This repo is the shared server engine. A deployed site typically looks like:

```
/var/www/<site>/
├── server/              # this repo, checked out here
├── server_overrides/    # optional site-owned Go overrides
├── static/              # the site's own static content
└── server/.env          # the site's own secrets/config (see Environment variables above)
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
ExecStartPre=/usr/bin/make -C /var/www/<site>/server build SITE_OVERRIDES=../server_overrides
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
make test              # complete local quality gate
make test-fast         # standard Go suite only
make test-race         # race detector
make test-integration  # build and exercise the real server process
make test-cover        # coverage report + 70% minimum gate
```

`make test` runs vet, the standard suite, the race detector, the tagged
process smoke test, and the coverage gate. Coverage artifacts are written to
`.build/coverage/coverage.out` and `.build/coverage/coverage.html`.

The normal suite uses temporary directories and in-memory/fake HTTP transports;
it does not contact the production mail relay, captcha service, or screenshot
service. The integration target binds an ephemeral loopback port, verifies
startup and the public routes against the built binary, then checks graceful
shutdown. Override the coverage threshold when needed with, for example,
`make test-cover COVERAGE_MIN=80`.
