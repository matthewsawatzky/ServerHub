# DESIGN — jobs, detached quick actions, and scheduling

Status: **all seven implementation phases are implemented** (`ssh.Conn`: shared
per-host control connection, keepalive, reconnect/backoff, session semaphore;
polls, Docker, and quick actions use it. `internal/jobs`: tmux job registry,
launch/log/status, stop/dismiss, observe streaming. Quick-action
`mode: detached` and the Jobs output panel with observe/attach/stop/dismiss.
`internal/scheduler`: `schedules.conf`, 10s ticker, target/tag resolution,
interval/day-at/5-field cron cadences, overlap, missed-run handling, run-now, and
bounded persistent history. Per-host
poll registry with cached static info/capabilities and boot-ID invalidation;
Docker image caching and live container-log streaming for remote and local
machines. `internal/probe`: on-demand ping (bidirectional between configured
servers), DNS, HTTP, traceroute, and explicit 5s iperf3 orchestration). Typed
probe schedules and persisted probe history are still future work. Read
`BUILD.md` for the current architecture and `AGENTS.md` for the dev/test rules.

Target platform: Ubuntu hosts with OpenSSH and tmux. `tmux` is a supported
dependency; no remote agent is planned.

---

## 1. Goals

- Keep **one control SSH connection per host** for polls, quick actions, job
  starts, log streaming, and network probes.
- Allow up to **four attached terminal connections** per host (handoff /
  interactive sessions), for a hard maximum of **five TCP connections per host**.
- Run long-lived or “leave it running” commands under **tmux on the remote**, so
  they survive ServerHub restarts and disconnects (not remote reboots).
- Let the user click **Observe** to stream a job’s output into a panel; no
  output streaming unless observed.
- Add a **scheduler page** under Dashboard for running quick actions or custom
  commands on one or many hosts.
- Expand polling with cheap, independently scheduled tasks (static system info,
  Docker images, capabilities, network probes).

## 2. Non-goals

- No remote agent or node process.
- No survival across remote reboots (use `systemd-run` later if that changes).
- No multi-user scheduling or per-user credentials.
- No agent-side collection while ServerHub is offline.

---

## 3. Connection and channel model

| Purpose | TCP connections | Channels |
|---|---|---|
| Control (polls, quick actions, jobs, observe, probes) | 1 | max 8 concurrent |
| Attached/handoff terminals | up to 4 | 1 each |
| Detached tmux jobs | 0 while detached | 0 |

`MaxSessions` (default 10) is per **connection**, not global. The control
connection therefore needs a semaphore; excess low-priority polls wait or skip.
Other tools/terminals on the host use their own connections and are unaffected.

Add a per-host connection owner, e.g. `ssh.Conn`:

- One dial at a time (mutex/singleflight); callers share the `*ssh.Client`.
- `NewSession()` for each operation; channels are cheap and concurrent-safe.
- Keepalive via `keepalive@openssh.com` every ~30s; invalidate and reconnect on
  failure.
- Reconnect with the existing offline backoff. Do not redial per poll/action.
- Session semaphore: 8 on the control connection; observation streams and
  long-running exec channels count against it.
- Cancellation closes the session, not the shared connection.

Terminal connections stay separate: one per attached terminal. They may attach
to a direct PTY or to a tmux session.

---

## 4. Jobs (tmux substrate)

A job is a remote tmux session plus a log file, not a held-open SSH channel.

```text
remote:
  ~/.serverhub/                 (0700)
  ~/.serverhub/jobs/            (0700)
  ~/.serverhub/jobs/<job-id>.log      (0600)
  ~/.serverhub/jobs/<job-id>.status   (0600, exit code; optional)

tmux session name:
  serverhub-<job-id>
```

Launch:

```sh
mkdir -p ~/.serverhub/jobs
tmux new-session -d -s serverhub-<job-id> -- sh -lc '<wrapped command>'
tmux set-option -t serverhub-<job-id> remain-on-exit on
tmux pipe-pane -t serverhub-<job-id> -o 'cat >> ~/.serverhub/jobs/<job-id>.log'
```

The wrapped command records the exit code before the pane is marked dead:

```sh
sh -lc '<command>; echo $? > ~/.serverhub/jobs/<job-id>.status'
```

Requirements and behaviour:

- `job-id`: validated `^[a-z0-9][a-z0-9-]*$`, max 64 chars. Prefer a readable
  prefix plus a short random/timestamp suffix, e.g. `nightly-update-20261019-a3f1`.
- `remain-on-exit on` keeps failed/crashed output inspectable. Exited sessions
  are shown as “exited”, with the recorded status, and can be dismissed/cleaned.
- One-shot jobs get a unique suffix. “Service-like” actions may set a fixed
  `session:` name and are rejected if that session already exists.
- An in-memory job registry maps job ID to server, schedule/action, session name,
  log path, start time, and status.
- ServerHub restart recovery: persist the registry (`jobs.json`, atomic 0600) and
  reconcile against `tmux list-sessions` on reconnect. Remote tmux is the source
  of truth; `jobs.json` is a cache. Recovered jobs are shown even if ServerHub
  lost their original request context.
- Retention: keep logs until the user dismisses the job or a retention policy
  removes them. Cap UI/streamed output through a ring/bounded buffer; do not
  read an unbounded log into memory. A size cap/cleanup for remote logs should be
  part of the implementation.

### Observe

- `Observe` opens one `tail -F <log>` exec channel on the control connection per
  **job**, then fans bytes out to all browsers watching that job.
- If the job has already exited, stream the remaining file and close.
- Cancel the tail when the last observer disconnects.
- Cap simultaneously observed jobs per host (suggested: 4) to protect the
  control connection’s channel budget.
- Output is rendered as text (never HTML) and audit-logged as an observation,
  not as command output.

### Attach / stop

- Attach opens `tmux attach -t serverhub-<job-id>` over one of the four handoff
  connections.
- Stop runs `tmux kill-session -t serverhub-<job-id>` and marks the job stopped.
- Detach closes the SSH connection; the tmux session keeps running.

---

## 5. Quick actions

Add a mode and a stable identity:

```yaml
quick_actions:
  - id: start-game               # optional; derived from label if absent
    label: "Start game server"
    command: "./game-server"
    mode: shell                  # shell (default) | detached
    session: game-server         # optional fixed session name
```

- `shell`: current behaviour. Run over the control connection, return buffered
  output.
- `detached`: start a tmux job and return “started in session …” immediately.
  Output is available only through Observe.
- `mode: detached` requires tmux; report a clear error if the capability probe
  did not find it.
- A foreground command that exits instantly leaves a dead tmux session, which
  remains inspectable because of `remain-on-exit`.
- Stable quick-action IDs let schedules survive label/command edits and list
  reordering. Migrate missing IDs on load (slugify label, de-duplicate).

### Output panel

- Move the output panel directly under the live-data section on the server page.
- Tabs/selectors per job: running/exited, duration, stop/attach/dismiss.
- Follow toggle, clear view, bounded scrollback.
- Do not auto-observe every run; only jobs the user chooses to observe are
  streamed. Normal quick actions can still show their final output inline.

---

## 6. Scheduler

### Definitions

Definitions live in their own `schedules.conf` next to `config.yaml` (same
pattern as `servers.conf`). Runtime state does not belong in config files.

```yaml
schedules:
  - id: nightly-updates
    name: "Nightly updates"
    enabled: true
    targets: [web-1, web-2]        # explicit IDs
    # tags: [prod]                 # optional alternative/union
    quick_action: start-game       # stable quick-action ID
    # command: "..."               # or a raw command
    every: 24h
    at: "03:30"                    # local time; optional
    overlap: skip                  # skip (default) | allow
    timeout: 30m
```

Supported cadence syntax: interval (`every`) plus optional local time-of-day
(`at`), or a 5-field numeric cron expression (`cron`) for advanced cases. The
web UI uses dropdowns for intervals/times; cron is the only free-text cadence.

### Semantics

- One scheduler goroutine ticks every ~10s and computes due schedules. No
  goroutine per schedule.
- Add small jitter across hosts so fleets do not start everything at once.
- Multi-target schedules create **one job per target**, with status aggregated
  per target in the UI.
- `overlap: skip` (default): if the previous job/session is still alive, do not
  start another. `allow` explicitly permits concurrent runs.
- Missed runs while ServerHub is down are **skipped and shown as missed**. Do not
  catch up automatically; an unattended apt upgrade after a restart is a
  surprise. Optional catch-up can be considered later.
- `Run now` bypasses the schedule but uses the same job path.
- Per-host start concurrency is bounded (suggested: 2 starts at a time); the
  control connection semaphore handles the rest.
- Audit start/finish by schedule ID, target, and status. Avoid logging the full
  command if it may contain secrets; the schedule definition in
  `schedules.conf` is already 0600.
- Targets by ID are stable. Tags are convenient but resolve at run time; warn if
  a target/tag disappeared.

### Page

A “Schedules” item directly under Dashboard:

- table: name, targets, action/command summary, cadence, next run, last status,
  enabled toggle.
- actions: run now, edit, delete, view history.
- create/edit modal: targets (IDs/tags), quick action picker or command, cadence,
  overlap, timeout.
- job output/history links to the same Observe panel.

---

## 7. Poll registry and expanded data

Replace the single stats branch with per-task registries on `ServerEntry`:

| Task | Interval | Notes |
|---|---|---|
| CPU/RAM/disk | 5s | existing compact payload |
| Docker containers | 30s | existing |
| Docker images | 5–10 min + manual refresh | refresh after pull/restart |
| System info | connect, then 6–24h | CPU model/cores, OS, RAM total, disks |
| Capabilities | connect | `tmux`, `docker`, `rsync`, `sftp`, `iperf3`, `sudo -n` |
| Network probes | explicit schedule | not on by default |

- Include `/proc/sys/kernel/random/boot_id` in the stats payload. A changed
  boot ID invalidates and refreshes static info.
- Static info is cached; do not re-run `lscpu` every five minutes.
- Per-task timeout and backoff. A failed static-info probe must not mark the
  whole host offline.
- Add jitter to scheduled polls.
- Keep the SSE card payload small; expose expanded info on the detail endpoint.
- Docker logs are streams, not polls: `docker logs -f` over the control
  connection while the view is open, cancelled on close.

---

## 8. Network probes

No remote agent. All checks are SSH exec sessions on the control connection.

- Ping/latency/loss: from A to B, and separately B to A (asymmetric).
- DNS: `dig` / `resolvectl` / `getent hosts` on the target.
- Internet: ICMP and/or `curl -o /dev/null -w ...` against configured targets.
- Traceroute: `traceroute` / `mtr` when installed.
- Bandwidth: iperf3 orchestration, on demand:
  1. On B, start `iperf3 -s -1 -p <random high port>` as a tmux job.
  2. On A, run `iperf3 -c <B> -p <port> -t 5 -J` and parse the JSON.
  3. The server exits by itself (`-1`); clean up the session if needed.

Rules:

- Detect `iperf3` as a capability and fail with a clear message.
- Bandwidth tests are explicit and time-capped; warn about data usage. Five
  seconds at 1 Gbps is roughly 600 MB per direction.
- Do not schedule bandwidth tests by default.
- Validate probe targets (IP or hostname); never interpolate unchecked input
  into a shell command.
- Typed probe schedules (`ping`, `dns`, `http`, `iperf`) can come after generic
  command schedules; store result JSON and render history.

---

## 9. Security

- Scheduled commands have the same power as quick actions; the UI/config are
  the trust boundary. Keep file modes 0600/0700 and validate all IDs/targets.
- Use `sudo -n` for apt-style actions so a missing NOPASSWD rule fails fast
  instead of hanging. Never pass passwords in argv or commands.
- Quote commands and session names; do not build tmux format strings from
  unvalidated data.
- Audit schedule starts/stops and job observations; never audit passwords or
  secrets.
- Observe/attach endpoints require a session like every other route and are
  subject to the existing Host/Origin guards.
- Remote job logs may contain secrets; keep them 0600 and clean them up.

---

## 10. Suggested implementation order

1. `ssh.Conn` per host: single dial, keepalive, reconnect/backoff, session
   semaphore; move stats/Docker/actions onto it.
2. Job registry + tmux launch/log/status + Observe streaming.
3. Quick-action `mode: detached` + output panel under live data.
4. Scheduler engine, `schedules.conf`, schedules page, run-now/history.
5. Poll registry + static info/capabilities caching.
6. Docker log streaming + image caching.
7. Network probes; optional iperf3 orchestration.

---

## 11. Open questions

- Cron expressions in v1, or interval/time-of-day only?
- Schedules via explicit IDs, tags, or both?
- Keep exited job logs until dismissed, or auto-prune after N days/MB?
- Should a schedule be allowed to target `all` hosts?
- Do we need typed probe results in v1, or are command schedules enough?
