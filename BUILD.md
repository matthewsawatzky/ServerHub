# BUILD.md — ServerHub

AI context document. Read this before touching the codebase. `AGENTS.md` has the
short operational rules (dev harness, state locations, Playwright UI testing);
this file has the full architecture and design rationale.

---

## What it is

ServerHub is a personal server management dashboard. It is a **single self-contained Go binary** that:

- Serves a web UI on a configurable local port
- Connects to one or more remote Linux servers over SSH
- Polls each server for CPU / memory / disk / container stats every 5 seconds
- Exposes a full PTY terminal to each server via xterm.js over WebSocket
- Monitors the local machine (the one running ServerHub) with the same stat model
- Stores app settings in `config.yaml` and the server inventory in `servers.conf` (OS config dir); both are readable and writable by hand and via the web UI

**Who runs it, and where:** either on a personal Mac/machine and accessed locally, or on any machine on a network (NAS, Pi, VPS) and accessed remotely via browser. The default bind is `127.0.0.1` (loopback only). It is **not** a SaaS product and has no multi-user model — it's a single-user personal tool.

---

## Directory structure

```
v4/
├── go/                         Main Go module (module name: serverhub)
│   ├── main.go                 Entry point: banner, config load, HTTP server, route wiring
│   ├── go.mod / go.sum
│   ├── internal/
│   │   ├── api/
│   │   │   ├── handlers.go     Hub struct + all HTTP/WS handlers
│   │   │   ├── guard.go        Host allowlist + origin/preflight middleware (audit V4)
│   │   │   ├── sse.go          Dashboard/detail SSE streams
│   │   │   └── audit.go        In-memory audit log (mutating actions only)
│   │   ├── auth/
│   │   │   └── auth.go         Password auth: bcrypt hash, sessions, middleware, rate limiter
│   │   ├── config/
│   │   │   └── config.go       YAML config + servers.conf split, Load/Save/SaveAll, migration, ValidateID
│   │   ├── storage/
│   │   │   └── storage.go      ONE place for state-dir resolution + durable/private file writes (0700/0600/atomic)
│   │   ├── ssh/
│   │   │   ├── client.go       SSH dial (TOFU host keys), command helpers, stats/container parsing
│   │   │   ├── conn.go         Per-host control connection: single dial, keepalive, reconnect, session semaphore
│   │   │   ├── images.go       Docker image parsing/collection
│   │   │   ├── info.go         Static host info + capability probe commands/parsers
│   │   │   ├── poller.go       ServerEntry: poll registry, static/capability/image caches, ring buffer
│   │   │   └── terminal.go     ProxyTerminal: SSH PTY ↔ WebSocket bridge (+ WS keepalive helper)
│   │   ├── jobs/
│   │   │   └── jobs.go         Detached tmux job registry, launch/log/status/stop, observe fan-out
│   │   ├── scheduler/
│   │   │   └── scheduler.go    schedules.conf loader, ticker, due/missed logic, run history
│   │   ├── probe/
│   │   │   └── probe.go        Network probe builders/parsers (ping/DNS/HTTP/traceroute; iperf3 JSON)
│   │   ├── local/
│   │   │   ├── actions.go      Local quick actions, Docker containers/images, log streaming
│   │   │   ├── collector.go    Local machine stats (Linux /proc, macOS sysctl/vm_stat, generic stub)
│   │   │   └── terminal.go     Local PTY shell ↔ WebSocket bridge
│   │   └── debug/
│   │       └── collector.go    Opt-in debug collector: logs, timings, runtime stats, WS broadcast
│   └── web/                    Embedded frontend (go:embed web/*)
│       ├── index.html          Main SPA shell
│       ├── login.html          Public page: onboarding wizard (3 steps) / login / opt-out info
│       ├── terminal.html       Full-page terminal view (vendored xterm.js, local assets only)
│       ├── debug.html          Debug panel (WS-fed live stats/logs/timings)
│       ├── app.js              Vanilla JS SPA — all UI logic, no build step
│       ├── icons.js            Inline SVG icon set
│       ├── style.css           All styles; theme accent colours via CSS custom properties
│       ├── assets/             Optional owner-provided images (e.g. onboarding-image.png, not in repo)
│       └── vendor/             Vendored xterm 5.3.0 + fit addon 0.8.0 (embedded, no CDN)
├── dist/                       Cross-compiled binaries (output of build.sh, not checked into git)
├── docs/
│   ├── releases/
│   │   └── README.md               Release-notes template + rules (notes: docs/releases/vX.Y.Z.md)
│   ├── examples/
│   │   ├── config.yaml.example     Annotated app-settings example
│   │   ├── servers.conf.example    Annotated server-inventory example
│   │   └── schedules.conf.example  Annotated schedule-definitions example
│   ├── design-jobs-scheduler.md    Design: connection model, tmux jobs, scheduler, probes
│   └── security-audit.md           Full security threat model and audit
├── docs_media/                 Screenshots for README embedding (demo image lives here)
├── build.sh                    Cross-compile script → dist/ (8 targets)
├── dev                         Dev harness: sandboxed build/run on port 7080 (test-config/)
├── release.sh                  Full release: git checks, tests, build, notes, tag, GitHub publish
├── AI.md                     How AI is used in this project (written by the AI itself)
├── LICENSE                     MIT
├── README.md                   End-user install + config reference
└── AGENTS.md                   AI context doc (operational rules; references BUILD.md)
```

Config directory layout (created on first run, `0700`):

```
<config dir>/                 macOS: ~/Library/Application Support/ServerHub
├── config.yaml               app settings: appearance, network, terminal, local card (0600)
├── servers.conf              remote server inventory (0600)
├── auth.json                 bcrypt password hash (0600)
├── sessions.json             active sessions (token SHA-256 hashes only, 0600)
├── jobs.json                 detached tmux job cache (remote tmux is the source of truth, 0600)
├── schedules.conf            schedule definitions (0600)
├── schedules-state.json      runtime schedule state: last runs, history, missed runs (0600)
└── known_hosts               pinned SSH host keys (TOFU, 0600)
```

---

## Stack

| Layer | Technology |
|---|---|
| Language | Go 1.25 |
| Web framework | `net/http` stdlib only |
| WebSocket | `github.com/gorilla/websocket` |
| SSH | `golang.org/x/crypto/ssh` (+ `ssh/knownhosts` for TOFU host key pinning) |
| Password auth | `golang.org/x/crypto/bcrypt` (already required via SSH — no new deps) |
| Config | `gopkg.in/yaml.v3` |
| Terminal I/O | `golang.org/x/term` (banner width detection) |
| Frontend | Vanilla JS + CSS, no framework, no build step |
| Terminal emulator | xterm.js 5.3.0 (vendored in `web/vendor/`, embedded) |
| Asset embedding | `//go:embed web/*` |
| Distribution | CGO_ENABLED=0 static binaries, 8 platforms |

---

## Architecture

### Central coordinator: `Hub`

`api.Hub` is the single stateful object for the server process. It holds:
- A pointer to the live `*config.Config` and the config file path
- A `map[string]*sshclient.ServerEntry` keyed by server ID
- A pointer to the `*local.Collector` for the local machine
- A `sync.RWMutex` that must be held for any read or write of the config or entries map

All add/update/delete server operations go through `Hub` methods, which update both the in-memory state and persist to disk via `config.Save()` under the lock.

### Auth

`api.Hub` has **no auth logic of its own**. Authentication lives in `internal/auth` and has three modes stored in `auth.json` (`"mode"`):

- `ModeUnset` (no auth.json) — first run. Every HTML request lands on the **onboarding wizard** at `/login` — three steps: the user's name (greeting; stored as `owner` in config), password yes/no, and an info page with an explainer image (`/onboarding-image`, optional file the owner drops at `go/web/assets/onboarding-image.{png,jpg,jpeg,webp,gif,svg}` and rebuilds to show). The wizard submits everything to `POST /api/onboarding` in one go. The local machine card stays hostname-based (`local_name` only changes via the card's rename).
- `ModePassword` — full password auth. See below.
- `ModeOptOut` (owner chose "No" in step 2) — requests pass through **without** sessions; the Host/Origin/preflight guard still applies to every route, so CSRF/DNS-rebinding/cross-origin WS stay blocked. A "no dashboard password" warning is printed at startup when bound beyond loopback. A password can be added later via `/api/auth/setup` (loopback-only) from the login page's "no password" view.

Password mode (`ModePassword`):

- `POST /api/auth/login` verifies bcrypt and issues an opaque 32-byte token in the `sh_session` cookie (HttpOnly, SameSite=Strict, Path=/, 30 days, `Secure` when behind TLS). Only `SHA-256(token)` is persisted in `sessions.json`.
- Sessions slide: each validated request extends expiry once ≥1 day has passed since the last extension (persisted at that point, not on every request).
- `auth.Middleware` wraps the whole mux in `main.go`: public allowlist is exactly `/login`, `/api/auth/status`, `/api/auth/login`, `/api/auth/setup`, `/api/onboarding`, `/onboarding-image`, `/api/ping`. Only **ModeOptOut** passes everything through. In ModeUnset (first run) and ModePassword everything else — **including WebSocket upgrades** — needs a valid session (401 JSON, or 302 to `/login?next=…` for HTML requests); in ModeUnset no session can exist yet, so every browser lands on the wizard until onboarding completes (stale cookies from a previous install/session are ignored — no sign-out dance required).
- `/api/onboarding` only runs (once) while `auth.json` does not exist and the client is loopback; it persists the user name (`owner`), the accent/shade (`hue`/`shade`) and creates `auth.json`. It also fixes the Hub's in-memory theme mirrors (`h.hue`/`h.shade`) in the same critical section — theme lives in **two places** (see warnings below).
- `GET /api/auth/status` additionally carries `hue`/`shade` (cosmetics only, no secrets) so the login page can pre-apply and preview the instance's look — the login page is fully themable and previews live while the wizard's sliders move.
- **Contrast rules:** text on accent surfaces uses `--accent-text` (computed from the accent's perceived luminance — dark text under ~L0.45, light otherwise). Text tracks the surface ramp in both halves of the shade range.
- `POST /api/auth/change` (session required) swaps the password after verifying the current one — wrong/current-verifications feed the shared rate limiter; on success **all sessions are revoked** and a fresh one is minted for the calling browser. Wrong "current password" answers `400` (not 401, so the SPA shows the inline error instead of bouncing to login).
- Settings → **Security** tab exposes the password UI: create (via `/api/auth/setup`, loopback-only, when none exists) or change (`/api/auth/change`, current password required). The Security tab also holds the bind-guidance text and the audit log; the General tab keeps appearance/prereqs/quick-actions.
- Login rate limiter is **global** (single user): 5 failures → 30 s lockout, each further failure doubles it, capped at 15 min; 429 + `Retry-After` even for the correct password; reset on success; in-memory only (resets on restart). It guards both login and password-change.
- Login failures/lockouts are appended to the audit log. The password value is never logged, audited, or persisted anywhere.

Handler chain (outermost → innermost) in `main.go`:

```
SecurityHeaders (CSP, nosniff, anti-framing, no-referrer)
  → Guard (Host allowlist + Origin/Referer + preflight marker on mutations)
    → auth.Middleware (pass-through only in ModeOptOut; session-checked otherwise)
      → debug.Middleware (captures authenticated requests only)
        → mux
```

### Per-server polling: `ServerEntry`

Each remote server has one `ServerEntry` with a small **poll registry** (`poller.go`). A single goroutine owns the tasks and their deadlines; there is no goroutine per task.

| Task | Cadence | Notes |
|---|---|---|
| Stats | 5s | CPU/RAM/disk/load, ring history, boot ID |
| Docker containers | 30s | skipped while the host is offline |
| Docker images | 10m | cached; manual refresh after pulls/builds; skipped offline |
| Static info | connect, then 6h | CPU model/cores, OS, kernel, arch, RAM, disks; cached |
| Capabilities | connect, then 6h | tmux, docker, rsync, sftp, iperf3, `sudo -n` |

- Stats failures back off 10s → 20s → 40s → 60s; the offline state also pauses containers/static/capabilities, which retry every 15s until stats recovers. Static/capability failures use their own capped backoff and never mark the host offline.
- Every task gets up to 10% jitter (capped at 5 minutes) so a fleet does not poll in lockstep.
- The stats payload includes `/proc/sys/kernel/random/boot_id`. A changed boot ID invalidates the cached static info and capabilities and schedules an immediate refresh.
- Static/capability results are exposed by `GET /api/server/<id>/info`; the dashboard SSE card stays compact. The detail page renders them in a **System** section.
- CPU percentage is derived from `/proc/stat` counters between consecutive stats samples, so there is no dependency on `top` or `mpstat`.
- Stats are stored in a ring buffer of 360 `StatPoint` values. `GetStats()` assembles the current snapshot plus the history slice — callers get a copy.

Docker images are cached for the detail page (`GET /api/server/<id>/images`) with a manual `POST .../images/refresh`. Container actions and successful shell quick actions wake the container/image tasks so a pull or restart is reflected quickly. The local machine exposes the same data through `/api/local/images`.

**Container logs are streams, not polls.** `GET /api/server/<id>/container/<name>/logs` (and `/api/local/container/<name>/logs`) upgrades to a WebSocket and runs `docker logs --tail N -f` over a control-connection session (local: the docker process). The stream is cancelled when the browser closes the socket, exits with the terminal request, and is capped at four concurrent log streams per server. Output is binary frames; the UI renders it as text with follow/clear/close.

To replace a server entry (on update), call `old.Stop()` (closes the `done` channel) then `StartOne(new)`.

### SSH connections

Each `ServerEntry` owns one `ssh.Conn` control connection per host (`internal/ssh/conn.go`). Stats polls, Docker polls, container actions, quick actions, reboot/shutdown all share it:

- Dial is lazy and single-flight: concurrent callers share one `*ssh.Client`; there is no redial per poll/action.
- A failed dial or connection is invalidated and enters exponential backoff (5s → 10s → 20s → 40s → 60s); callers fail fast during the backoff window. The poller's offline stats backoff still governs how often a dead host is retried.
- `keepalive@openssh.com` runs every 30s with a 10s timeout; a failed keepalive invalidates the connection so the next poll/action redials.
- Control sessions are capped by a per-host semaphore of 8. Cancelling a command closes only that SSH session, never the shared connection.
- `ServerEntry.Stop()` closes the control connection along with the polling goroutine.

**Terminal sessions stay separate:** `ProxyTerminal` dials its own connection for the lifetime of the WebSocket, opens a PTY session, then bridges WebSocket messages ↔ SSH stdin/stdout. Terminal resize events arrive as JSON text frames (`{"type":"resize","cols":N,"rows":M}`); all other WebSocket frames are treated as raw stdin bytes.

**SSH host keys are verified (TOFU).** On the first connection to a host its key is appended to `<config dir>/known_hosts` (0600) and logged; every later connection verifies it and fails with an explicit "host key mismatch" error if the key changes. The old behaviour is opt-in-only via `server.insecure_ignore_host_key: true`, which is logged as a warning at startup. Setting `HostKeyCallback` on a new dial must go through `ssh.SetHostKeyPolicy` / `ssh.hostKeyCallback` — do not reintroduce `InsecureIgnoreHostKey` as a default.

**WebSocket connections are bounded** (audit V7): every upgraded connection has `SetReadLimit` (1 MiB terminals / 1 KiB debug stream), a 25 s control ping with a 60 s read deadline (browsers auto-pong), and interactive shells share one global cap of 8 sessions (HTTP 429 to excess). The HTTP server has `ReadHeaderTimeout: 10s` and `IdleTimeout: 120s`; deliberately **no global WriteTimeout** (it would kill SSE/WS).

### Detached jobs (tmux)

`internal/jobs.Registry` runs long-lived commands under tmux on the remote host so they survive ServerHub restarts and disconnects:

- Remote state lives in `~/.serverhub/jobs/`: `<job-id>.log`, `<job-id>.status` (exit code), and a transient `<job-id>.go` marker. The tmux session is named `serverhub-<job-id>`.
- Launch waits for `pipe-pane` to attach before the command starts (marker file), sets `remain-on-exit on`, and records the exit code in the status file.
- `jobs.json` in the config dir is a cache; remote tmux/status files are the source of truth. Status is refreshed lazily when jobs are listed or observed (dead panes become `exited`, missing sessions become `stopped`), and proactively by a change hook: the registry fires a callback whenever a job stops running, which the scheduler uses to fold real exit codes into schedule history. A background reconciler (`Hub.ReconcileScheduleJobs`, 30s ticker in main.go) refreshes servers that still have running schedule-sourced jobs so nothing waits for a UI visit.
- Job IDs follow the server-ID charset. One-shot jobs get a label/date/random suffix; service-like jobs may request a fixed `session`, rejected if the registry or remote tmux already has it.
- `Observe` opens one `tail -F` stream per job on the host's control connection and fans chunks out to all WebSocket subscribers (bounded per-subscriber buffers; slow clients drop chunks). The tail stops one second after the job writes its status file. At most four jobs per host are observed at once. Cancelling the last observer closes only the tail session.
- Quick actions can set `mode: detached`: the action starts a job and returns immediately. The server page's **Jobs** panel lists running/exited jobs and offers Observe, attach (a normal terminal connection running `tmux attach`), stop, and dismiss.
- Routes: `GET/POST /api/server/<id>/jobs`, `GET/DELETE /api/server/<id>/jobs/<job-id>`, `POST .../stop`, and `GET .../observe` (WebSocket, binary frames). Attach uses `/terminal/<id>?job=<job-id>` → `/api/server/<id>/terminal?job=<job-id>`.

### Scheduler

`internal/scheduler` owns `schedules.conf` and a runtime state file (`schedules-state.json`). One goroutine ticks every 10 seconds; there is no goroutine per schedule.

- Definitions use `targets` (explicit server IDs) and/or `tags` (resolved at run time), one of `quick_action` (stable quick-action ID) or `command`, a cadence (`every`, optional daily `at: "HH:MM"`, or a 5-field `cron` expression), `overlap` (`skip` default / `allow`), and optional `timeout` (wrapped with `timeout N sh -c …`). The Schedules modal uses server multi-select, dropdown intervals/times, and a cron text field; typed server names are resolved to IDs against the current inventory.
- Each run starts one detached job per target through the same jobs registry, with a small per-target jitter and a per-host start cap of 2. Fixed quick-action sessions naturally reject duplicates.
- `overlap: skip` checks the schedule's last job for that target, then atomically claims a per-(schedule, server) start slot (`claimStart`/`releaseStart`) before launching, so two ticks close together cannot both slip through the check.
- Every started schedule job gets an acute watcher (`watchJob`): the scheduler polls its status every 5s for up to a minute so quick commands fold their real outcome (exit code → `done`/`failed`, re-aggregated) into history within seconds. Long-lived jobs fall back to the 30s reconciler.
- History status reflects reality: "started" is transient; finished jobs update the entry and (if newest) the schedule's headline status/summary.
- Runs missed while ServerHub was down are **not caught up**: the scheduler advances past them, records a `missed` history entry, and continues with the next occurrence. A 30s grace window covers normal tick lateness. Resuming a paused schedule re-baselines and records how many runs were skipped while paused (`trigger: "pause"`).
- Cadence math is DST-aware: whole-day intervals step wall-clock days (`addInterval`), shorter intervals keep exact real-time gaps; cron never fires twice during a fall-back's repeated hour (`cronWallLabel` guard).
- Invalid `schedules.conf` entries are skipped at load, not fatal; the messages surface via `GET /api/scheduler/status` and a banner on the Schedules page. Deleted occurrences are tracked in a bounded `skipped` list (100 max).
- Routes: `GET/POST /api/schedules`, `PUT/DELETE /api/schedules/<id>`, `GET/PATCH /api/schedules/<id>`, `POST /api/schedules/<id>/run` (dispatches async), `/delete-occurrence`, `GET /api/schedules/occurrences`, `/preview`, and `GET /api/scheduler/status`.

### Network probes

On-demand probes run as SSH exec sessions on the host's control connection (no remote agent) and are exposed on the server detail page:

- **Ping** — `ping -c 4`; if the target is a configured server ID, it runs in both directions (A→B and B→A) and reports both summaries.
- **DNS** — `getent hosts`, falling back to `dig`/`nslookup`.
- **HTTP** — `curl` with `--max-time 10`, reporting status code, time, and bytes.
- **Traceroute** — `traceroute` with an `mtr` fallback; raw output is kept.
- **iperf3** — explicit, time-capped (5s) test between two configured servers. The target server runs `iperf3 -s -1 -p <random high port>` as a tmux job; the source runs `iperf3 -c … -J`, then the job is dismissed. Both hosts must have `iperf3` (capability probe, with a live `command -v` fallback). The UI warns about data usage before running.
- Targets are validated as IPs/hostnames or http(s) URLs; validated values are shell-quoted. `POST /api/server/<id>/probe` returns a JSON result with status, duration, summary, bounded output, and structured data.
- Typed probe schedules (ping/DNS/HTTP/iperf with persisted result history) are not implemented; generic command schedules can still run probe CLIs.

### Local machine collector

`local.Collector` mirrors `ssh.ServerEntry`'s stat model (same `ServerStats` and `StatPoint` types) so the frontend rendering path is identical for local and remote cards. Collection is OS-aware: Linux reads `/proc/*` directly; macOS shells out to `sysctl`/`vm_stat`/`top`; other OSes return an empty stub.

### Debug system

`debug.Global` is a process-wide opt-in collector (disabled by default, zero overhead when off). Enable it from the web UI (Settings → Debug) or via `POST /api/debug/enable`. **Debug endpoints are local-only** — they additionally require a loopback client IP (`127.0.0.1`/`::1`) on top of the session cookie, and are documented as a local feature. When enabled it:
- Intercepts `log.Printf` output via `NewLogInterceptor` (also forwarded to stderr, so warnings are never silent)
- Times every HTTP request via `debug.Middleware` (wraps the root mux, inside the auth middleware — so it records authenticated requests only)
- Records SSH dial/bytes counters and per-server poll timings
- Truncates every recorded string to 512 runes (logs and URL paths — audit V9)
- Broadcasts a `Frame` diff to all `/api/debug/stream` WebSocket subscribers every 500ms

### Frontend SPA

`app.js` is a single vanilla JS file with no bundler. Views are toggled by show/hide in `index.html`; the URL is kept in sync with the view (`/`, `/schedules`, `/settings`, `/<server-id>`) via `history.pushState`/`popstate`, and `main.go` serves `index.html` for those SPA paths on reload. The dashboard and per-server detail views subscribe to SSE streams (`/api/dashboard/events`, `/api/server/<id>/events`); REST calls are used for mutations. The theme accent colour is a single CSS custom property (`--accent`) set via `applyTheme()`.

The dashboard splits cards into **Favorites** and **Servers**. The local machine is favorited by default; remote servers can be starred from cards or the sidebar. Favorites are stored as `favorite` on `servers.conf` hosts and in the `local` block of `config.yaml`, toggled through `PATCH /api/server/<id>/favorite` and `PATCH /api/local`. A persistent top-right search filters the sidebar/cards by name, ID, host, tags, or notes; `Cmd/Ctrl+L` focuses it and Enter opens the first match. The detail page order is stats → actions → quick actions → jobs → containers → network → images → system → notes.

**XSS rules (enforced — see docs/security-audit.md V3/V5):**
- Server IDs must match `^[a-z0-9][a-z0-9-]*$` (max 64 chars) — enforced by `config.ValidateID` on both `POST /api/servers` and on every config load; `__local__` is reserved.
- Remote-controlled strings (container names, load values) are sanitised at the boundary in the ssh package and additionally escaped by `esc()` client-side (which also escapes `'`).
- **Data-derived markup must never use inline `onclick="...${data}..."` strings.** Render `data-*` attributes and use the delegated listeners installed by `attachDelegatedHandlers()` (sidebar, cards, containers, quick actions, action row).
- `apiFetch` re-directs to `/login?next=…` on 401; mutating fetches must carry `X-Requested-With: XMLHttpRequest` (the guard middleware enforces it).
- The sidebar **Sign out** button only renders when the instance is password-protected (`has_password` from `/api/auth/status`) — it starts hidden in the markup and is revealed in `init()`; opt-out instances have no sessions to end.

### Config persistence

**All file storage goes through `internal/storage`** — one package owns the state-directory resolution (`storage.Dir()`, including the dev-build override) and the durability/privacy invariants: directories `0700`, files `0600`, writes atomic (`temp + fsync + rename` in the same directory). Callers marshal their own content (`config.Save`/`SaveServers` → YAML, auth → JSON via `storage.WriteJSON`, known_hosts appends via `storage.Append`) and keep their own semantics (e.g. auth treats a corrupt sessions.json as empty). **Never write ServerHub state files with raw `os.WriteFile`/`os.OpenFile`** — use storage helpers so permissions/atomicity can't regress.

The state is split across two YAML files, both 0600 in the 0700 config directory:

- `config.yaml` — app settings only (`server`, `hue`/`shade`, global `terminal`, `local` card). `Config.Servers` carries `yaml:"-"`, so it can never leak back into this file.
- `servers.conf` — the remote server inventory (`servers:` list), next to the config path (including custom paths and dev builds).

`config.Load()` reads both. Existing installs with a legacy `servers:` block in `config.yaml` are migrated on load: the inventory is written to `servers.conf`, the legacy block is stripped, and the migration is logged. If both files contain an inventory, `servers.conf` wins and the legacy copy is removed. Nothing is written if the legacy inventory fails validation.

Call sites choose the file(s) they change: `Save` (app settings), `SaveServers` (inventory), `SaveAll` (both). Server mutations and expired quick-action metadata use `SaveAll`; appearance/onboarding/local-card edits use `Save`. On save failure the in-memory change is rolled back and the API returns 500. Auth state never touches either file — it lives in the sibling `auth.json` / `sessions.json` files. Both YAML files are hand-editable, but changes require a restart (hot-reload is planned) and IDs are validated on load (`config.ValidateID`).

### Tests

`go test ./...` covers the storage package (permissions, atomic writes, JSON round-trip, append), auth (setup/login/session expiry/revocation/rate limiting), the API guard (Host/Origin/preflight rules, security headers), config validation/round-tripping, the SSH connection lifecycle (single dial, backoff, keepalive, session limits, streaming), and the tmux job registry (launch/stop/refresh/dismiss, persistence, observe fan-out, plus a real local tmux lifecycle test), the scheduler (definition validation, next-due/at/cron computation, missed runs, overlap, manual runs, and definition/state round-tripping), the poll registry/static-info parsers (task backoff, jitter, offline skipping, disk/capability parsing, and probe shell syntax), Docker image parsing/log-tail limits, and probe validation/command/parser coverage. `stress_test.sh` is a manual HTTP load test and is marked for removal.

---

## Running and building

### Development (local run)

```sh
./dev                # build ./serverhub for this machine (fast, no cross-compile)
./dev race           # with the Go race detector
./dev run [--args]   # build then run; args go to the binary (e.g. ./dev run /tmp/cfg.yaml)

# or directly:
go run . [/optional/path/to/config.yaml]
```

**Dev builds are fully sandboxed from the live install, and every run starts clean:** `./dev` deletes the previous binary and `./test-config/`, then bakes `-X serverhub/internal/storage.devDir=<repo>/test-config` into the fresh binary — so all dev state (config, auth, known_hosts) lives only in a newly seeded `./test-config/` (gitignored), and the onboarding wizard shows on every dev launch. The seeded dev config listens on port **7080** (the live default is 7070) with `open_browser: false`. Rebuilding the live binary with `./build.sh` or `go build` produces a normal binary with no override.

Default state paths (auto-created on first run; `servers.conf` sits next to `config.yaml`):
- macOS: `~/Library/Application Support/ServerHub/`
- Linux: `~/.config/ServerHub/`
- Windows: `%AppData%\ServerHub\`

### Production build (all platforms)

```sh
./build.sh          # outputs to dist/
```

Targets: `darwin/amd64`, `darwin/arm64`, `linux/amd64`, `linux/arm64`, `linux/armv7`, `linux/armv6`, `windows/amd64`, `windows/arm64`.

All builds use `CGO_ENABLED=0 -trimpath -ldflags="-s -w"`.

### Single-platform build

```sh
cd go
go build -o serverhub .
./serverhub
```

---

## Current state

### Done
- First-run onboarding wizard on `/login`: name the machine → password yes/no → "what you're looking at" info page (optional explainer image from `web/assets/`)
- Password protection (bcrypt, 30-day sliding sessions, global rate limiter, sign-out) — optional, recorded opt-out if declined
- Host allowlist + origin/preflight CSRF guards on all routes (WebSocket upgrades included)
- SSH host key verification via TOFU known_hosts (`insecure_ignore_host_key` opt-out)
- SSH polling with 30-min sparkline history (CPU, mem, disk)
- Docker container list + start/stop/restart, cached image list with manual refresh, and live container log streaming
- Full PTY terminal over WebSocket (vendored xterm.js, no CDN)
- Quick actions (arbitrary SSH commands, output returned inline with a Stop button)
- Detached tmux jobs: registry with `jobs.json` cache, launcher with log/status files, stop/dismiss, status reconciliation, and observe streaming over WebSocket
- Detached quick actions (`mode: detached`) with a Jobs panel under the live stats (observe/attach/stop/dismiss, follow + clear)
- Scheduler: `schedules.conf` definitions, 10s ticker, target/tag resolution, interval/day-at/cron cadences, overlap skip/allow, missed-run handling, run-now, bounded persistent history, and a Schedules page
- Favorites (local by default), dashboard Favorites/Servers groups, persistent server URLs, and a Cmd/Ctrl+L server search
- On-demand network probes (bidirectional ping, DNS, HTTP, traceroute) and explicit 5s iperf3 orchestration between configured servers
- Reboot / shutdown with confirmation
- Add / edit / delete servers via UI (persists to servers.conf)
- Accent hue + surface shade controls (WCAG-checked contrast), persisted
- Live dashboard and detail updates over SSE
- Poll registry with cached static system info (CPU/OS/kernel/RAM/disks) and capabilities (tmux/docker/rsync/sftp/iperf3/sudo -n), refreshed on boot-ID change and every 6h; System section on the detail page
- In-memory audit log (mutating actions in current session, capped at 2000 entries)
- Local machine card (backend fully implemented; shows the machine running ServerHub)
- Debug panel (opt-in; live runtime stats, HTTP timings, log stream; loopback-only)
- Cross-platform static binary build
- OS-appropriate config directory (macOS / Linux / Windows)

### Planned / in progress (from TODO)
- **Typed probe schedules and persisted probe history** — on-demand probes and iperf3 are implemented; typed probe schedule entries/results remain future
- **Config hot-reload** — hand-editing config.yaml or servers.conf requires a restart; automatic detection and reload is planned
- **Config backup** — plan to keep an `old_conf` copy after each write so a bad hand-edit can be rolled back
- **Durable auth hardening** — move session storage / password to OS keychain (accepted risk: anyone with read access to the config dir can copy sessions.json)
- **Deeper run log** — schedule history is bounded (20 entries); a durable off-file log may follow

---

## Things to avoid / watch out for

**SSH host key verification** — TOFU pinning is now the default: first connect records the key in `<config dir>/known_hosts`, later connects fail on change. Do not regress to `InsecureIgnoreHostKey` as a default; the only opt-out is `server.insecure_ignore_host_key: true` (logged as a warning at startup). When a legitimate key change happens, the user removes the stale line from known_hosts (or `ssh-keygen -R`, but the file here is its own format — same line format as OpenSSH).

**Auth is single password + session cookie (optional by owner choice)** — session tokens are stored as SHA-256 hashes in `sessions.json` next to config.yaml. The owner may decline a password in the first-run wizard (`ModeOptOut`); in that mode everything passes the auth middleware (guard still applies) and a startup warning fires when bound non-loopback. Accepted owner risk: anyone who can read the config directory can copy the session file and is effectively logged in (file access already implies config/SSH key access). Do not "improve" the design ad hoc — see docs/security-audit.md "Solution design — password auth". Never log, audit, or echo the password value. The login rate limiter is in-memory only: lockouts reset on restart.

**Middleware ordering is load-bearing** — the handler chain is Guard → auth.Middleware → debug.Middleware → mux. Debug must stay inside auth (it must only capture authenticated requests); Guard must stay outside auth (host/origin checks apply to public routes like /login too). Do not reorder.

**Data-derived markup must not use inline event handlers** — see the XSS rules in the Frontend SPA section. Server ID charset is enforced by `config.ValidateID`; keep it strict (new sinks = new escaping work).

**Vendored xterm** — `web/vendor/` holds xterm 5.3.0 + fit addon 0.8.0 (MIT). There must be no CDN references in `web/`. If upgrading, re-vendor and re-embed; do not add runtime network deps.

**Stat command is Linux-specific** — `statsCmd` in `ssh/client.go` uses `/proc/uptime`, `/proc/stat`, `/proc/meminfo`, and `/proc/loadavg`. Remote servers must be Linux. The local collector handles macOS separately, but there is no equivalent for macOS/BSD *remote* servers. Do not assume remote stat collection works on non-Linux.

**Audit log is in-memory** — `audit.go` holds entries in a slice that resets on restart. Persistence is planned but not implemented; do not treat it as durable.

**`TODO.md` is intentionally ignored by AI** — the file starts with an explicit `AI AGENTS: IGNORE THIS FILE` directive. Do not act on items in it unless the user explicitly asks.

**`stress_test.sh` is dead weight** — it exists but is marked for removal. Do not maintain or extend it.

**`.gitignore` is in place** — `dist/`, `test-config/`, the dev binary, and local-only files (`TODO.md`, `stress_test.sh`) are untracked. Do not commit them.

**Releases are scripted** — `release.sh` owns the release flow (clean-tree checks, tests, build, checksums, tag, GitHub publish). Never hand-edit `dist/` artifacts and never reuse or move a published tag. See `AGENTS.md` → "Releasing a version".

**Theme state lives in two places** — `Hub.hue`/`Hub.shade` (runtime mirrors used by the API) and `Hub.cfg.Hue`/`Hub.cfg.Shade` (written to disk). Both must be updated together; see `handleAppConfig` and `handleOnboarding` for the pattern. On a failed save both are rolled back.

**Shell quoting in container actions** — `shellQuote()` in `handlers.go` does single-quote escaping for Docker container names passed to SSH. Do not bypass this when constructing SSH commands.

---

## Glossary

| Term | Meaning |
|---|---|
| `ServerEntry` | In-memory object representing one remote server; owns its poll goroutine and ring buffer |
| `Hub` | Central HTTP handler struct; owns all `ServerEntry` objects and the live config |
| `StatPoint` | One time-stamped sample of CPU/mem/disk percentages, stored in the ring buffer |
| `RawStats` | Parsed output of the remote `statsCmd` shell one-liner (before unit conversion) |
| `ContainerList` | Timestamped snapshot of `docker ps -a` output for one server |
| `quick_actions` | User-defined named SSH commands shown as buttons on the server detail page |
| `__local__` | Reserved server ID for the local machine card; rejected as a server ID by `config.ValidateID` |
| `sh_session` | Session cookie (opaque 32-byte token; only its SHA-256 is stored) |
| `Guard` | Host-allowlist + origin/preflight middleware wrapping every route |
| `known_hosts` | `<config dir>/known_hosts` — TOFU-pinned SSH host keys, 0600 |
| `Frame` | Incremental debug payload broadcast to debug WebSocket subscribers every 500ms |
| `Snapshot` | Full debug state sent once to a newly connected debug WebSocket client |
| ring buffer | Fixed 360-slot circular array of `StatPoint`; `ringHead` + `ringCount` track position |
| `Job` | One detached tmux job: id, server, session, log/status paths, status, exit code |
| `jobs.json` | Cache of jobs in the config dir; remote tmux/status files win on refresh |
| Observe | `tail -F` stream of a job's log over the control connection, fanned out to WebSocket subscribers |
| `Schedule` | A definition in `schedules.conf`: targets/tags, action or command, cadence, overlap, timeout |
| `schedules.conf` | Schedule definitions next to `config.yaml`; managed from the Schedules page |
| `schedules-state.json` | Runtime scheduler state: last scheduled run, last status, bounded history, missed runs |
| Run now | Manual execution that bypasses the timer/overlap but records history like a scheduled run |
| `StaticInfo` | Cached host inventory: boot ID, CPU/OS/kernel/arch, RAM, disks |
| `Capabilities` | Cached remote tools: tmux, docker, rsync, sftp, iperf3, `sudo -n` |
| poll registry | Per-host task list (stats/containers/static/capabilities) with jitter and per-task backoff |
| `ImageList` | Cached `docker images` snapshot (10m) with manual refresh |
| Container logs | `docker logs -f` stream over a control-connection session, capped per host |
| Probe | On-demand network check on the server detail page (ping/DNS/HTTP/traceroute/iperf3) |
| iperf3 | Explicit 5s bandwidth test: one-shot tmux server on B + JSON client on A |
