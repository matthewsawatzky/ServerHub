# Security

ServerHub is built for one person controlling their own machines. It assumes
the operator is trusted and everything else is not. The full threat model and
audit live in [security-audit.md](security-audit.md); this page is the
operational summary.

## First-run setup

- On the very first launch (from the machine running ServerHub) the browser
  walks you through a short **setup wizard**:
  1. **What do you want to be called?** — your name, used to greet you on the
     dashboard. The machine's card shows its hostname (renameable later).
  2. **Password — yes or no.** Recommended; if you decline, the instance runs
     unprotected (see below).
  3. An **info page** showing what everything is.
- First-run setup only works from the machine itself (`127.0.0.1` / `::1`).
  Opening a not-yet-set-up instance from another device shows instructions
  instead.
- You can personalise the wizard's explainer image by dropping your own at
  `go/web/assets/onboarding-image.png` and rebuilding.

## Password protection

- The password is stored as a **bcrypt hash** (`auth.json`, mode `0600`) next
  to `config.yaml` — never in the config itself. The password value is never
  logged or audited.
- Logging in issues a session cookie (HttpOnly, SameSite=Strict) valid for
  **30 days** or until you sign out; sessions survive restarts. Only token
  SHA-256 hashes are stored (`sessions.json`).
- A global rate limiter locks out brute force: 5 wrong passwords → 30 s
  lockout, doubling with each further failure (cap 15 min).
- **Changed your mind later?** From the dashboard host, open the login page
  after choosing "No password" and set one there. If a password is already set
  and needs changing, use **Settings → Security → Change password**. If the
  password is lost entirely: stop ServerHub, delete `auth.json` (and
  `sessions.json`), restart, and set a new one locally. Physical file access
  is the recovery factor by design.
- **Running without a password** is a deliberate mode: every request still
  passes the host/origin/CSRF guards, but anyone who can *reach* the port can
  use the dashboard — only do this on a loopback or trusted encrypted overlay,
  and never bind beyond loopback without one.

## Network exposure

- **Default bind is `127.0.0.1`** — the dashboard is only reachable from your
  local machine.
- All endpoints are additionally guarded: only requests whose `Host` header
  matches the bind address / `localhost` / `127.0.0.1` are accepted (this
  blocks DNS-rebinding and other hijacks), and cross-origin or header-less
  mutating requests are rejected.
- To share with your tailnet, set `bind` to your Tailscale IP (e.g.
  `100.x.x.x`). A prominent warning is printed at startup: **everything
  travels as plaintext HTTP** (terminal keystrokes included), so a
  non-loopback bind must sit inside an encrypted overlay
  (Tailscale/WireGuard/SSH tunnel).
- **Never set `bind: "0.0.0.0"`** unless you know what you're doing and have
  additional access controls (VPN, firewall rules, reverse proxy with auth).
- Hostname-based URLs are not accepted by the Host guard — use the IP you bind
  to, or localhost.

## SSH

- SSH private keys are never logged or sent over the network.
- **Host keys are verified (trust-on-first-use):** the first connection records
  the host's key in `<config dir>/known_hosts` (0600); later connections fail
  with an explicit "host key mismatch" error if the key changes. To recover
  after a legitimate key change, verify the new key out-of-band and delete the
  host's line from that file.
- Set `server.insecure_ignore_host_key: true` only if you explicitly want the
  old, unverified behaviour (it is logged as a warning at startup).
- All SSH connections use a 3-second dial timeout. Stats poll every 5 seconds;
  an unreachable host backs off up to 60 seconds instead of being retried at
  full rate.

## Data permissions

- The config directory is created mode `0700`; `config.yaml`, `servers.conf`,
  `auth.json`, `sessions.json`, `jobs.json`, `schedules.conf` and
  `schedules-state.json` are written mode `0600` (legacy installs are
  corrected automatically on the next load/save).
- `servers.conf` contains hostnames, users, key paths and quick-action
  commands; `schedules.conf` contains scheduled commands — treat the directory
  as sensitive.