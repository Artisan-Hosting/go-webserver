# Contact Form + Cap Captcha — Frontend Integration Reference

This document exists so that a frontend (or an AI assistant building one) can
implement the site's contact form and its [Cap](https://trycap.dev) captcha
widget **entirely from this file and this repo's Go source** — no additional
research against trycap.dev should be necessary. It covers exactly what this
server expects and returns; where Cap's own docs describe options this
backend doesn't use, they're omitted.

If you are an AI assistant asked to build the contact form frontend for a
site that runs this server, read this file in full, then read `contact.go`
and `captcha.go` to confirm nothing has drifted, then implement.

## 1. What this server provides

| Method & path | Purpose |
|---|---|
| `GET /api/captcha-config` | Tells the frontend whether captcha is enabled, and if so, which Cap endpoint to talk to. |
| `POST /api/contact` | Submits the contact form. Validates captcha (if enabled), emails the submitter + site owner. |

Captcha is **opt-in per deployment** — it's controlled entirely by whether
the server has `CAPTCHA_API_ENDPOINT` and `CAPTCHA_SECRET_KEY` set (see
README → Environment variables). The frontend must not hardcode either
assumption; always check `/api/captcha-config` first and branch on its
`enabled` field.

### 1.1 `GET /api/captcha-config`

No auth, no query params. Response body (`Content-Type: application/json`,
always `200 OK`):

```json
{
  "enabled": true,
  "endpoint": "https://cap.example.com/<site-key>/"
}
```

- `enabled: false` → `endpoint` will be `""`. Don't render the captcha widget
  at all; don't require a token on submit.
- `enabled: true` → `endpoint` is the exact string to pass as the Cap
  widget's `data-cap-api-endpoint` attribute (see §3). It already includes
  the trailing site-key path segment — use it verbatim, don't append or
  modify it.

This endpoint's response reflects live server config, so fetch it at page
load / component mount rather than caching it at build time.

### 1.2 `POST /api/contact`

Request: `Content-Type: application/json` — **not** a form-encoded POST, and
**not** `multipart/form-data`. Body shape (Go struct tags are the literal
JSON keys expected):

```ts
{
  name: string;           // required, non-empty after trim
  email: string;          // required, non-empty after trim
  business?: string;      // optional
  site_url?: string;      // optional
  ClientMessage?: string; // optional; server fills a default if blank
  OwnerMessage?: string;  // optional; leave unset, see note below
  capToken?: string;      // the Cap solve token — required only if captcha is enabled
}
```

Notes:
- `name` / `email` are the only hard requirements. Missing/blank either one
  (after trimming whitespace) gets a `400`.
- `ClientMessage` defaults server-side to `"No project details were
  provided."` if blank — the frontend doesn't need to enforce a minimum
  itself, though a nicer UX will still ask the user for project details.
- `OwnerMessage` is an internal escape hatch (accepts quoted-printable
  encoded text for legacy callers) — **leave it unset from a new frontend**;
  the server builds a sensible owner-notification body from the other fields
  automatically.
- `capToken` is the token obtained from the Cap widget's `solve` event (see
  §3). Send it as `capToken` in the JSON body — Cap's own docs describe a
  hidden form field named `cap-token` for native `<form>` POSTs, but that
  convention **does not apply here** since this endpoint reads JSON, not
  form fields. Extract the token yourself and put it under the `capToken`
  key.

Success response: `200 OK`, `Content-Type: application/json`:

```json
{"status":"ok"}
```

Both the submitter's confirmation email and the owner's notification email
have already been dispatched (fire-and-forget) by the time this returns.

#### Error responses

Every non-2xx response is `Content-Type: text/plain; charset=utf-8` (Go's
`http.Error` always sets this, regardless of what the body text looks like —
see the `-sim-error` note below). Read the body as text; don't assume
`response.json()` will succeed on an error path.

| Status | When | Body (example) |
|---|---|---|
| `400` | Wrong content / missing fields | `bad request`, `name and email are required` |
| `400` | Captcha enabled, `capToken` missing | `captcha verification required` |
| `400` | Captcha enabled, token rejected by Cap | `captcha verification failed` |
| `405` | Method other than `POST` | `method not allowed` |
| `502` | Captcha enabled, but the Cap instance itself couldn't be reached | `captcha verification unavailable` |

A generic frontend error handler keyed on status code is sufficient; there's
no machine-readable error code field in the body today.

#### Simulating failures for UI testing (`-sim-error`)

This server has a dev-only flag, `-sim-error`, for exercising the frontend's
error handling without actually breaking anything real. Start the server
with e.g. `./bin -sim-error=503` and every `POST /api/contact` will short-circuit
into that failure mode instead of doing real work:

| Value | Behavior |
|---|---|
| `500` | Immediately returns `500` with a JSON-shaped error body (still `text/plain` content type). |
| `503` | Immediately returns `503`, same pattern. |
| `429` | Returns `429` with a `Retry-After: 30` header. |
| `timeout` | Never responds — the connection just hangs until the client's own fetch/XHR timeout fires. |
| `drop` | Abruptly closes the raw TCP connection (simulates a dropped connection / network error, not a clean HTTP response). |

Use these to drive out a complete `ERROR_MESSAGES`-style mapping in the
frontend (see §4) — including the states that never produce a normal HTTP
response (`timeout`, `drop`), which are easy to forget to handle.

## 2. Cap widget primer (only what this integration needs)

Cap is a self-hosted, proof-of-work captcha. The widget is a web component,
`<cap-widget>`, distributed as the `cap-widget` package.

```html
<script src="https://cdn.jsdelivr.net/npm/cap-widget@latest"></script>
```

Or `import "cap-widget"` if you're bundling (React/Vue/Svelte all work the
same way once the custom element is registered).

### Required attribute

- `data-cap-api-endpoint` — set this to the `endpoint` string returned by
  `GET /api/captcha-config` (§1.1), unmodified.

### Events

| Event | Payload | Use |
|---|---|---|
| `solve` | `{ token: string }` | Fires when the user completes the challenge. `e.detail.token` is what you send as `capToken`. |
| `progress` | `{ progress: number }` | Optional — drive a progress indicator. |
| `error` | `{ message: string }` | Widget-side failure (e.g. couldn't reach the Cap endpoint). Show an error state; the form can't be submitted until `solve` fires. |
| `reset` | `{}` | Fires after `.reset()` is called (see below). |

### Resetting after a failed submit

Cap tokens are single-use. If `POST /api/contact` comes back with a captcha
error (or any error, to be safe — the user may need to retry), call
`.reset()` on the widget element and wait for a fresh `solve` before allowing
resubmission:

```js
document.querySelector("cap-widget").reset();
```

### Optional attributes worth knowing about

- `data-cap-hidden-field-name` (default `cap-token`) — irrelevant here since
  we read the token from the `solve` event and send JSON, not a native form
  POST, but worth knowing so you don't accidentally rely on the auto-injected
  hidden field.
- `data-cap-i18n-*` — override widget copy per-locale if the site is
  localized.

Full attribute/event/CSS-variable reference: [trycap.dev widget
docs](https://trycap.dev/guide/widget.html) — only needed for cosmetic
customization beyond the above.

## 3. Wiring it together

Sequence for a contact form on a page served by this backend:

1. On mount, `fetch("/api/captcha-config")`.
2. If `enabled === false`: render the form without any captcha widget. Submit
   the JSON body from §1.2 with no `capToken` field (or omit it — the server
   only checks it when captcha is enabled).
3. If `enabled === true`:
   a. Render `<cap-widget data-cap-api-endpoint="{endpoint}">` inside (or
      alongside) the form.
   b. Disable the submit button until a `solve` event has fired; store
      `e.detail.token`.
   c. On submit, `POST /api/contact` with `capToken` set to the stored token.
   d. On any error response, re-enable the form for editing and call
      `widget.reset()` so the user gets a fresh challenge before retrying —
      a used/stale token will just fail captcha verification again.
4. On success (`{"status":"ok"}`), show a confirmation state and stop
   accepting further submissions (or reset the form for a new submission,
   per the site's UX preference).

## 4. Minimal working example (vanilla JS)

```html
<form id="contact-form">
  <input name="name" required />
  <input name="email" type="email" required />
  <input name="business" />
  <input name="site_url" type="url" />
  <textarea name="ClientMessage"></textarea>

  <div id="captcha-slot"></div>

  <button type="submit" id="submit-btn" disabled>Send</button>
  <p id="form-error" hidden></p>
</form>

<script type="module">
  const form = document.getElementById("contact-form");
  const submitBtn = document.getElementById("submit-btn");
  const errorEl = document.getElementById("form-error");
  const captchaSlot = document.getElementById("captcha-slot");

  let capToken = null;
  let widget = null;

  const ERROR_MESSAGES = {
    400: "Please check the form and try again.",
    405: "Something went wrong on our end. Please try again shortly.",
    502: "Verification service is temporarily unavailable. Please try again shortly.",
    network: "Couldn't reach the server. Check your connection and try again.",
    timeout: "The request is taking too long. Please try again.",
  };

  function showError(message) {
    errorEl.textContent = message;
    errorEl.hidden = false;
  }

  async function initCaptcha() {
    const res = await fetch("/api/captcha-config");
    const { enabled, endpoint } = await res.json();

    if (!enabled) {
      submitBtn.disabled = false; // no captcha gate
      return;
    }

    await import("https://cdn.jsdelivr.net/npm/cap-widget@latest");

    widget = document.createElement("cap-widget");
    widget.setAttribute("data-cap-api-endpoint", endpoint);
    captchaSlot.appendChild(widget);

    widget.addEventListener("solve", (e) => {
      capToken = e.detail.token;
      submitBtn.disabled = false;
    });
    widget.addEventListener("error", () => {
      capToken = null;
      submitBtn.disabled = true;
      showError("Captcha failed to load. Please refresh and try again.");
    });
  }

  form.addEventListener("submit", async (e) => {
    e.preventDefault();
    errorEl.hidden = true;
    submitBtn.disabled = true;

    const fd = new FormData(form);
    const payload = {
      name: fd.get("name"),
      email: fd.get("email"),
      business: fd.get("business") || undefined,
      site_url: fd.get("site_url") || undefined,
      ClientMessage: fd.get("ClientMessage") || undefined,
      ...(capToken ? { capToken } : {}),
    };

    const controller = new AbortController();
    const timer = setTimeout(() => controller.abort(), 15000);

    try {
      const res = await fetch("/api/contact", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
        signal: controller.signal,
      });
      clearTimeout(timer);

      if (res.ok) {
        form.hidden = true; // or show a "thanks" state
        return;
      }

      const text = await res.text();
      showError(ERROR_MESSAGES[res.status] || text || "Something went wrong.");
    } catch (err) {
      clearTimeout(timer);
      showError(err.name === "AbortError" ? ERROR_MESSAGES.timeout : ERROR_MESSAGES.network);
    } finally {
      if (widget) {
        capToken = null;
        widget.reset();
        submitBtn.disabled = true; // re-enabled by the next `solve` event
      } else {
        submitBtn.disabled = false; // no captcha gate to wait on
      }
    }
  });

  initCaptcha();
</script>
```

This example deliberately avoids a framework so it maps 1:1 onto React/Vue/
Svelte equivalents — swap the `createElement`/`addEventListener` calls for
the framework's `on:solve`/`onsolve`/`@solve` prop as shown in [Cap's widget
docs](https://trycap.dev/guide/widget.html#framework-usage-examples), keep
everything else (endpoint fetch, JSON body shape, error handling, reset-on-
failure) as-is.

## 5. Checklist

- [ ] Fetches `/api/captcha-config` before deciding whether to render the widget.
- [ ] Never assumes captcha is on or off — always branches on `enabled`.
- [ ] Sends `POST /api/contact` as JSON (`Content-Type: application/json`), not a form POST.
- [ ] Token is sent as `capToken` in the JSON body, not relied on via the widget's auto-injected `cap-token` hidden field.
- [ ] Submit is disabled until `solve` fires (when captcha is enabled).
- [ ] Widget is `.reset()` after any failed submission.
- [ ] Handles `400`, `405`, `502` with user-facing messages.
- [ ] Handles a request that never responds (client-side timeout) and one that drops the connection (network error) — test both with `-sim-error=timeout` and `-sim-error=drop`.
- [ ] Treats error bodies as plain text, not JSON.
