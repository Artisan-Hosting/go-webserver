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
| `PROMETHEUS_URL` | *(unset)* | Base URL of a Prometheus holding blackbox_exporter probe results, e.g. `http://127.0.0.1:9090`. Leaving this or `STATUS_SERVICES` unset disables `/api/status`. **Do not expose this Prometheus publicly** — proxying it is the point of the endpoint. |
| `PROMETHEUS_BEARER_TOKEN` | *(unset)* | Optional `Authorization: Bearer` token, if Prometheus sits behind auth. Never returned to clients. |
| `STATUS_SERVICES` | *(unset)* | The allowlist: one `<probe target>\|<display name>` per line. A probe target absent from this list is never published, so this doubles as the public/private boundary. Blank lines and `#` comments are ignored; trailing slashes are normalized. |
| `STATUS_SELECTOR` | `job=~"blackbox_.+_probe_[ab]"` | PromQL label matchers selecting the blackbox probe jobs. Must exclude jobs that scrape exporter internals rather than probe results. |
| `STATUS_DEGRADED_WINDOW` | `1h` | How far back a location's missed checks are accumulated when deciding Degraded. |
| `STATUS_DEGRADED_MISS_RATIO` | `0.05` | Share of checks one location may miss inside that window before the service reads as Degraded. Must be in `[0, 1)`. |
| `STATUS_DAY_MISS_MINUTES` | `5` | How long a location may be missing checks within one day before that day is marked on the history strip. |
| `STATUS_DEGRADED_MS` | *(unset)* | Optional secondary Degraded signal: every location up but slower than this many milliseconds. Off by default. |
| `STATUS_CACHE_TTL` | `30s` | How long a status snapshot is served before refreshing. Match your `scrape_interval`; polling faster cannot produce new information. |

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
| `GET /api/status` | `statusHandlerWithDependencies` | Public service status derived from blackbox_exporter probes in Prometheus. See [Service status](#service-status). |
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

## Service status

`/api/status` publishes uptime for a fixed list of monitored services. It exists
so that Prometheus itself never has to be reachable from a browser: the API is
unauthenticated and returns every metric the deployment scrapes, including
internal hostnames and the full probe target list.

Set `PROMETHEUS_URL` and `STATUS_SERVICES` to enable it. With either unset the
endpoint returns `503 {"enabled": false}` and a status page can render its own
"unavailable" state.

```sh
PROMETHEUS_URL=http://127.0.0.1:9090
STATUS_SERVICES="
https://www.artisanhosting.net/|Website
https://cloud.artisanhosting.net/status.php|Cloud Storage
"
```

### What it publishes

Only the display name and the derived numbers. The probe target URL, the
`instance` label (which is the exporter, not the site), and the job name are
never included, so the payload cannot be used to enumerate what is monitored.

```json
{
  "enabled": true, "generated_at": "...", "stale": false,
  "overall": "ok", "windows": ["24h", "7d", "30d"],
  "services": [{
    "name": "Website", "state": "ok",
    "http_status": 200, "latency_ms": 84,
    "probes_up": 2, "probes_total": 2, "last_probe": "...",
    "uptime": {"24h": 1, "7d": 0.9998, "30d": 0.9994},
    "history": [
      {"availability": 1},
      {"availability": 0.9722, "locations_affected": 1, "locations_total": 2, "http_status": 502},
      null
    ]
  }]
}
```

### Assumptions about the scrape config

The queries aggregate `by (target)`, not `by (instance)`. This suits a blackbox
setup where `relabel_configs` move the probed URL into a `target` label and
rewrite `__address__` to the exporter's address — the common multi-exporter
layout, in which `instance` identifies the exporter rather than the probed site.
If your setup keeps the URL in `instance`, set `STATUS_SELECTOR` accordingly and
adjust the aggregation.

`state` is deliberately not derived from instantaneous probe agreement. Probing
from several places over a real network produces a steady trickle of isolated
failures, and reacting to each one makes the page cry wolf. A location has to
have missed more than `STATUS_DEGRADED_MISS_RATIO` of its checks over
`STATUS_DEGRADED_WINDOW` before the service reads as `warn`; below that, a
location failing right now is ignored so long as another location still sees the
site and latency is within `STATUS_DEGRADED_MS`. `bad` stays immediate — if
nothing can reach the target, that is not noise.

At a 30s scrape the defaults mean a location must miss more than ~3 minutes of
the last hour to trip, while a single failed check is ~0.8% and passes unnoticed.
`miss_rate` and `locations_missing` on each service report what tripped it.

`uptime` is the mean of `probe_success` averaged across probes, so a
single-vantage failure counts partially against uptime. `history` carries 30
daily buckets, oldest first; a bucket with no data in Prometheus is `null` rather
than `0`, so gaps do not render as outages. Both need TSDB retention at least as
long as the window being reported.

A day that was not clean also carries what monitoring saw, assembled from what
blackbox already exports:

| Field | Meaning |
|---|---|
| `locations_affected` / `locations_total` | How much of the probe redundancy had a bad day. |
| `no_response` | At least one probe never got an HTTP response — `probe_http_status_code` of `0`, i.e. a DNS, TCP or TLS failure rather than a bad status. |
| `http_status` | The worst status code seen, reported only when it is `>= 400`. |
| `content_failed` | `probe_failed_due_to_regex` fired: the target answered, but with the wrong body. |

A clean day carries nothing but `availability`, since there is no failure to
describe. Fields are omitted rather than zeroed, so absent metrics (a module that
does not export `probe_failed_due_to_regex`, say) simply produce no claim.

### Operational notes

A snapshot is cached for `STATUS_CACHE_TTL` and refreshes are single-flighted, so
a burst of page loads produces one set of queries. If a refresh fails, the last
good snapshot is served with `"stale": true` rather than blanking the page; only
an empty cache plus an unreachable Prometheus returns `502`.

Allowlist drift is logged once per distinct problem at `status:` — a configured
service Prometheus has no data for (dropped rather than published as a false
outage), and a probed target missing from the allowlist. An empty grid is almost
always a `target` label that does not match `STATUS_SERVICES`; the logs name the
exact strings involved.

The 30-day window scans roughly 86k samples per series at a 30s scrape interval,
and the history query evaluates five such aggregations per step. That is fine for
a few dozen targets behind the snapshot cache, but a large target list is worth
backing with recording rules.
