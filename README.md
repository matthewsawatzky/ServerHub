# ServerHub

A self-contained personal server management dashboard. Single Go binary — no external daemon, no Node.js, no Docker required to run it.

Open a browser, see all your Linux servers at a glance, manage Docker containers, and drop into a full terminal — all from one place.

---

## Prerequisites

### SSH key

Generate a key if you don't have one:

```sh
ssh-keygen -t ed25519 -C "serverhub"
```

Copy it to each server:

```sh
ssh-copy-id -i ~/.ssh/id_ed25519 user@192.168.1.10
```

### Docker access (optional)

ServerHub polls `docker ps -a` over SSH. The SSH user needs permission to talk to the Docker daemon. Add the user to the `docker` group:

```sh
sudo usermod -aG docker $USER
# log out and back in for the change to take effect
```

ServerHub invokes `docker` directly (not through `sudo`), so a scoped NOPASSWD sudoers rule for `/usr/bin/docker` alone is not enough. Use the `docker` group (or rootless Docker with the appropriate socket permissions).

If the user has no Docker access, ServerHub will show an empty container list rather than erroring.

---

## Installation

```sh
git clone <repo>
cd serverhub/go

# Build (the Go module lives in go/)
go build -o ../serverhub .

# Run — creates a starter config in the OS config directory on first launch
cd ..
./serverhub
```

The binary embeds the entire frontend — no separate web server or file serving needed.

Config lives in the OS config directory (`~/Library/Application Support/ServerHub` on macOS, `~/.config/ServerHub` on Linux, `%AppData%\ServerHub` on Windows). To specify a custom config path:
```sh
./serverhub /path/to/config.yaml
```
The server inventory is read from `servers.conf` in the same directory as the config file.

A commented example is in `docs/config.yaml.example` (app settings) and `docs/servers.conf.example` (inventory). Existing installs with a `servers:` block in `config.yaml` are migrated automatically on first load (the block is moved to `servers.conf` and removed from `config.yaml`).

---

## Config reference

### config.yaml — how ServerHub runs

```yaml
server:
  port: 7070              # port to listen on
  bind: "127.0.0.1"      # interface to bind (see Security section)
  open_browser: true      # auto-open browser on launch
  insecure_ignore_host_key: false  # true disables SSH host key verification (not recommended)

hue: 120                  # accent colour (0-360)
shade: 20                 # surface shade (0=black … 100=white)
owner: ""                 # your name (set by the setup wizard; greets you on the dashboard)

terminal:                 # global web-terminal defaults
  background: "#0e0e10"
  foreground: "#e0e0e8"
  accent: "#639922"

local:                    # the machine running ServerHub
  name: ""                # empty = system hostname
  tags: []
  notes: ""
  quick_actions: []
```

### servers.conf — remote server inventory

```yaml
servers:
  - id: my-server         # unique identifier (used in URLs)
    name: "my-server"     # display name
    host: "192.168.1.10"  # IP or hostname
    port: 22              # SSH port
    user: "youruser"      # SSH username
    key: "~/.ssh/id_ed25519"  # path to private key (~ is expanded)
    tags: ["homelab"]     # optional labels shown on cards
    notes: "Free text notes shown on the detail page"
    quick_actions:
      - label: "Restart nginx"
        command: "sudo systemctl restart nginx"
      - label: "Show disk usage"
        command: "df -h"
```

### Quick actions

Quick actions are shell commands that run as the configured SSH user. Their combined output is shown inline when the command finishes (with a Stop button while it runs). Use them for runbooks: backups, deployments, service restarts, log tails, etc.

Set `mode: detached` to launch the command as a tmux job instead. Detached actions return immediately, keep running if ServerHub restarts, and stream output only when you click **Observe** in the server's Jobs panel (with **attach** to open the tmux session and **stop**/**dismiss** actions). Add an optional fixed `session` for service-like commands; one-shot actions get a unique suffix.

Actions get a stable `id` derived from the label the first time they are loaded. You normally don't need to set it by hand, but keeping one makes schedules and renames stable.

```yaml
quick_actions:
  - id: pull-images
    label: "Pull latest images"
    command: "docker compose -f ~/compose.yml pull && docker compose -f ~/compose.yml up -d"
  - label: "Tail app log (last 50)"
    command: "journalctl -u myapp.service -n 50 --no-pager"
  - label: "Start game server"
    command: "./game-server"
    mode: detached
    session: game-server
```

### Schedules

Schedules live in `schedules.conf` next to `config.yaml` and are managed from the **Schedules** page. Each run starts one detached tmux job per target, so output is available in the server's Jobs panel. Targets can be explicit server IDs, tags (resolved when the schedule runs), or both.

```yaml
schedules:
  - id: nightly-updates
    name: "Nightly updates"
    enabled: true
    targets: [web-1, web-2]
    tags: [prod]                  # optional union with targets
    quick_action: pull-images     # stable quick-action ID...
    # command: "..."              # ...or a raw command
    every: 24h
    at: "03:30"                   # optional daily local time
    overlap: skip                 # skip (default) | allow
    timeout: 30m
```

Cadence can be an interval (`every`), a daily time (`every: 24h` plus `at`), or a 5-field cron expression (`cron: "0 3 * * *"`). The web UI uses server multi-select and dropdowns for intervals/times; cron is the only free-text cadence.

`overlap: skip` will not start another run while the previous job for that target is still running. Runs missed while ServerHub was offline are skipped and shown as **missed**; they are not caught up automatically. **Run now** starts immediately without changing the cadence. Runtime history is stored in `schedules-state.json` (never in the definitions file) and is bounded per schedule.

---

## Security

### First-run setup

- On the very first launch (from the machine running ServerHub) the browser walks you through a short **setup wizard**:
  1. **What do you want to be called?** — your name, used to greet you on the dashboard. The machine's card shows its hostname (renameable later on the machine itself).
  2. **Password — yes or no.** Recommended; if you decline, the instance runs unprotected (see below).
  3. An **info page** showing what everything is. You can personalise it by dropping your own explainer image at `go/web/assets/onboarding-image.png` (jpg/jpeg/webp/gif/svg also work) and rebuilding.
- First-run setup only works from the machine itself (`127.0.0.1` / `::1`). Opening a not-yet-set-up instance from another device shows instructions instead.

### Password protection

- The password is stored as a **bcrypt hash** (`auth.json`, mode `0600`) next to `config.yaml` — never in config itself.
- Logging in issues a session cookie (HttpOnly, SameSite=Strict) valid for **30 days** or until you sign out; sessions survive ServerHub restarts.
- A global rate limiter locks out brute force: 5 wrong passwords → 30 s lockout, doubling with each further failure (cap 15 min).
- **Changed your mind later?** From the dashboard host, open the login page after choosing "No password" and set one there. If a password is already set and needs changing, use **Settings → Security → Change password**. If the password is lost entirely: stop ServerHub, delete `auth.json` (and `sessions.json`), restart, and set a new one locally. Physical file access is the recovery factor by design.
- **Running without a password** is a deliberate mode: every request still passes the host/origin/CSRF guards, but anyone who can *reach* the port can use the dashboard — only do this on a loopback or trusted encrypted overlay, and never bind beyond loopback without one.

### Network exposure

- **Default bind is `127.0.0.1`** — the dashboard is only reachable from your local machine.
- All endpoints are additionally guarded: only requests whose `Host` header matches the bind address / `localhost` / `127.0.0.1` are accepted (this blocks DNS-rebinding and other hijacks), and cross-origin or header-less mutating requests are rejected.
- To share with your tailnet, set `bind` to your Tailscale IP (e.g. `100.x.x.x`). A prominent warning is printed at startup: **everything travels as plaintext HTTP** (terminal keystrokes included), so a non-loopback bind must sit inside an encrypted overlay (Tailscale/WireGuard/SSH tunnel).
- **Never set `bind: "0.0.0.0"`** unless you know what you're doing and have additional access controls (VPN, firewall rules, reverse proxy with auth).
- Hostname-based URLs are not accepted by the Host guard — use the IP you bind to, or localhost.

### SSH

- SSH private keys are never logged or sent over the network.
- **Host keys are verified (trust-on-first-use):** the first connection records the host's key in `<config dir>/known_hosts` (0600); later connections fail with an explicit "host key mismatch" error if the key changes. To recover after a legitimate key change, verify the new key out-of-band and delete the host's line from that file.
- Set `server.insecure_ignore_host_key: true` only if you explicitly want the old, unverified behaviour (it is logged as a warning at startup).
- All SSH connections use a 3-second dial timeout. Stats poll every 5 seconds; an unreachable host backs off up to 60 seconds instead of being retried at full rate.

### Data permissions

- The config directory is created mode `0700`; `config.yaml`, `servers.conf`, `auth.json`, `sessions.json`, `jobs.json`, `schedules.conf` and `schedules-state.json` are written mode `0600` (legacy installs are corrected automatically on the next load/save). `servers.conf` contains hostnames, users, key paths and quick-action commands; `schedules.conf` contains scheduled commands — treat the directory as sensitive.

---

## Features

- First-run onboarding wizard (name, optional password, guided intro)
- Optional password protection (bcrypt, 30-day sessions, brute-force lockout)
- Live CPU / memory / disk stats with 30-minute sparkline history
- Cached system info and capability probes (OS, CPU, kernel, RAM, disks, tmux/docker/rsync/sftp/iperf3/sudo) on the server detail page
- Docker container management (start / stop / restart), cached image list with manual refresh, and live logs (remote and local)
- Full PTY terminal via xterm.js over WebSocket (vendored — no CDN, works offline)
- SSH host key verification (trust-on-first-use known_hosts)
- Per-server quick actions with inline output and a Stop button
- Detached tmux quick actions with observe / attach / stop / dismiss
- Schedules for quick actions or commands across servers/tags, with overlap control, run-now, and bounded history
- On-demand network probes (ping both directions between servers, DNS, HTTP, traceroute) and explicit 5-second iperf3 bandwidth tests
- Starred favorites (the local machine by default), with dashboard Favorites/Servers groups
- Persistent server URLs (`/<server-id>`) that survive reloads and back/forward navigation
- Server search with a persistent top-right search bar (`Cmd/Ctrl+L`), filtering the sidebar and dashboard
- Reboot / shutdown with inline confirmation
- Accent hue and surface shade controls (WCAG-checked contrast)
- In-session audit log of all mutating actions
- Zero runtime dependencies — single static binary
