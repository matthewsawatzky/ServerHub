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

ServerHub polls `docker ps -a` over SSH. The SSH user needs permission to talk to the Docker daemon. Two options:

**Option 1 — docker group** (persistent):
```sh
sudo usermod -aG docker $USER
# log out and back in for the change to take effect
```

**Option 2 — scoped NOPASSWD sudo** (more controlled):
Add to `/etc/sudoers` via `visudo`:
```
youruser ALL=(ALL) NOPASSWD: /usr/bin/docker
```

If the user has no Docker access, ServerHub will silently show an empty container list rather than erroring.

---

## Installation

```sh
git clone <repo>
cd serverhub

# Copy and edit config
cp config.yaml.example config.yaml
$EDITOR config.yaml

# Build
go build -o serverhub .

# Run
./serverhub
```

The binary embeds the entire frontend — no separate web server or file serving needed.

To specify a custom config path:
```sh
./serverhub /path/to/config.yaml
```

---

## Config reference

```yaml
server:
  port: 7070              # port to listen on
  bind: "127.0.0.1"      # interface to bind (see Security section)
  open_browser: true      # auto-open browser on launch
  insecure_ignore_host_key: false  # true disables SSH host key verification (not recommended)

theme: "green"            # accent colour: green | blue | purple | coral
owner: ""                 # your name (set by the setup wizard; greets you on the dashboard)

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

Quick actions are shell commands that run as the configured SSH user. Their output streams back to the UI inline. Use them for runbooks: backups, deployments, service restarts, log tails, etc.

```yaml
quick_actions:
  - label: "Pull latest images"
    command: "docker compose -f ~/compose.yml pull && docker compose -f ~/compose.yml up -d"
  - label: "Tail app log (last 50)"
    command: "journalctl -u myapp.service -n 50 --no-pager"
```

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
- **Changed your mind later?** From the dashboard host, open the login page after choosing "No password" and set one there. If a password is already set and needs changing: stop ServerHub, delete `auth.json` (and `sessions.json`), restart, and set a new one locally. Physical file access is the recovery factor by design.
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
- All SSH connections use a 3-second dial timeout and reconnect on failure.

### Data permissions

- The config directory is created mode `0700` and `config.yaml` is written mode `0600` (legacy installs are corrected automatically on the next load/save). The config contains hostnames, users, key paths and quick-action commands — treat the directory as sensitive.

---

## Features

- First-run onboarding wizard (name, optional password, guided intro)
- Optional password protection (bcrypt, 30-day sessions, brute-force lockout)
- Live CPU / memory / disk stats with 30-minute sparkline history
- Docker container management (start / stop / restart)
- Full PTY terminal via xterm.js over WebSocket (vendored — no CDN, works offline)
- SSH host key verification (trust-on-first-use known_hosts)
- Per-server quick actions with output streaming
- Reboot / shutdown with inline confirmation
- Theme switcher (green / blue / purple / coral)
- In-session audit log of all mutating actions
- Zero runtime dependencies — single static binary
