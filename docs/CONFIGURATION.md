# Configuration reference

Everything is plain YAML, read and written by both the web UI and by hand.
ServerHub never talks over the network except SSH to your servers.

Where state lives:

```
<config dir>/                 macOS: ~/Library/Application Support/ServerHub
                              Linux: ~/.config/ServerHub
                              Windows: %AppData%\ServerHub
├── config.yaml               app settings (0600)
├── servers.conf              remote server inventory (0600)
├── auth.json                 bcrypt password hash (0600)
├── sessions.json             active sessions (token SHA-256 hashes only, 0600)
├── jobs.json                 detached tmux job cache (remote tmux is source of truth, 0600)
├── schedules.conf            schedule definitions (0600)
├── schedules-state.json      runtime schedule state: last runs, history, missed runs (0600)
└── known_hosts               pinned SSH host keys (TOFU, 0600)
```

To use a different directory, pass the config path explicitly: `./serverhub /path/to/config.yaml`.
Existing installs with a `servers:` block inside `config.yaml` are migrated
automatically on first load (the block moves to `servers.conf`).

Annotated, copy-paste examples live in `docs/examples/`:
[`config.yaml.example`](examples/config.yaml.example),
[`servers.conf.example`](examples/servers.conf.example),
[`schedules.conf.example`](examples/schedules.conf.example).

---

## config.yaml — app settings

```yaml
server:
  port: 7070              # port to listen on
  bind: "127.0.0.1"       # interface to bind (see Security)
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

---

## servers.conf — server inventory

The inventory lives next to `config.yaml`, never inside it.

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

Server IDs may only contain `[a-z0-9-]`.

## Quick actions

Quick actions are shell commands that run as the configured SSH user. Their
combined output is shown inline when the command finishes (with a Stop button
while it runs). Use them for runbooks: backups, deployments, service restarts,
log tails, etc.

Set `mode: detached` to launch the command as a tmux job instead. Detached
actions return immediately, keep running if ServerHub restarts, and stream
output only when you click **Observe** in the server's Jobs panel (with
**attach** to open the tmux session and **stop**/**dismiss** actions). Add an
optional fixed `session` for service-like commands; one-shot actions get a
unique suffix.

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

Actions get a stable `id` derived from the label the first time they are
loaded. You normally don't need to set it by hand, but keeping one makes
schedules and renames stable.

## Schedules

Schedules live in `schedules.conf` next to `config.yaml` and are managed from
the **Schedules** page (calendar, hourly day view, or list). Each run starts
one detached tmux job per target, so output stays available in the server's
Jobs panel. Targets can be explicit server IDs, tags (resolved when the
schedule runs), or both; `__local__` targets the machine running ServerHub.

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

### Cadence

Exactly one of:

| Field | Meaning |
|---|---|
| `date` + `at` | one-off run at a date and `HH:MM` |
| `days` + `at` | weekly / custom days: `days: [mon..sun]`, `at: "HH:MM"` |
| `every` | Go duration (`10s` … `90m` … `24h`), min 10s |
| `every` + `at` | anchored recurring run (e.g. `every: 24h` + `at: "03:30"`) |
| `cron` | 5-field numeric cron expression (`cron: "0 3 * * *"`), free text |

Whole-day intervals (`24h`, `48h`, …) keep their clock time across DST
changes; shorter intervals keep exact real-time gaps between runs.

### Behaviour

- `overlap: skip` (default) does not start another run while the previous job
  for that target is still running; `allow` starts regardless.
- `timeout` wraps the command in a per-run timeout (e.g. `30m`).
- Runs missed while ServerHub was offline are skipped and shown as
  **missed**; they are not caught up automatically.
- **Run now** starts the schedule immediately without changing its cadence
  (and bypasses the overlap check).
- History is honest: it starts as "started" and then folds in the real outcome
  from the job — `success`/`error` once exit codes are known.
- Pausing and resuming re-baselines the cadence and records how many runs were
  skipped while paused.
- Individual occurrences (or an occurrence and all after it) can be deleted
  from the calendar; deleted occurrences are recorded, not lost.
- Runtime history lives in `schedules-state.json`, never in the definitions
  file, and is bounded per schedule.

Invalid entries in `schedules.conf` (e.g. edited by hand with a typo) do not
stop the app: they are skipped at load and reported in a warning banner on the
Schedules page.