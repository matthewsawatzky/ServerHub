# AGENTS.md — ServerHub

Read this before touching the codebase. This is the operational handbook: how
to work in this repo, what must never be broken, and the flows for the asks
this project gets. The deep architecture and design rationale live in
`BUILD.md` — read both before non-trivial changes. (`AI.md` explains the
human/agent split in this project, briefly and disrespectfully.)

**Read order for a fresh agent:**
1. This file (operational rules + security invariants)
2. `BUILD.md` (architecture: Go backend, embedded SPA, jobs, scheduler)
3. `docs/SECURITY.md` + `docs/security-audit.md` (threat model — most
   mitigations there are deliberate; do not "improve" them ad hoc)
4. `docs/CONFIGURATION.md` + `docs/examples/` (what users configure)
5. `docs/design-jobs-scheduler.md` (agreed model for jobs/schedules/probes —
   do not diverge from it without discussing the change)

## Recipes — what you'll be asked to do

| The ask | What to do |
|---|---|
| "fix X and release" / "release the next update" | Fix → verify → release notes → commit/push → `./release.sh vX.Y.Z` (see [Releasing](#releasing-a-version)) |
| "add a feature" | Understand existing patterns first (`BUILD.md`), implement, verify, then `go test ./...` + a Playwright pass if UI |
| "the UI looks/behaves wrong" | Reproduce with `./dev run` + Playwright screenshot, fix in `go/web/`, verify visually |
| "add a server-side endpoint" | Follow `go/internal/api/handlers.go` patterns: guard passes every request; use `writeJSON`; validate inputs; audit mutating actions via `LogAction` |
| Hand-edit conf files break things | Invalid schedule entries are skipped + surfaced (`GET /api/scheduler/status`, Schedules-page banner). Do not make load errors fatal again |
| Investigate a crash/hang | `./dev race` build; reproduce via API or Playwright; check debug panel (Settings → Debug, loopback-only) |

## Hard rule: never test against the live install

The live instance uses the OS config directory (`~/Library/Application Support/ServerHub`,
`~/.config/ServerHub`, `%AppData%\ServerHub`) and is **off-limits** while this
version is in beta.

Use the dev harness for every build and test:

```sh
./dev            # build for this machine, state sandboxed in ./test-config/
./dev run        # build, then run on port 7080 (binds the developer's tailnet IP, not loopback)
./dev race       # build with the Go race detector
```

`./dev` deletes the previous dev binary and **wipes `test-config/`** on every
run, then bakes `-X serverhub/internal/storage.devDir=<repo>/test-config` into
the binary. That override redirects *all* state — `config.yaml`, `servers.conf`,
`auth.json`, `sessions.json`, `jobs.json`, `schedules.conf`,
`schedules-state.json`, `known_hosts` — into `test-config/`. The dev config
listens on port **7080** (live default is 7070) and does not open a browser.
The dev instance is password-less; curl it on the address `./dev` prints.

Important: passing `./serverhub /some/other/config.yaml` does **not** redirect
auth/session/host-key state. Unless the binary was built with the `storage.devDir`
ldflag above, those files still resolve to the live OS config directory. When in
doubt, use `./dev`. (Backgrounding `./dev run` without a TTY works: the script
feeds the server a non-EOF stdin.)

`test-config/`, `serverhub`, and `dist/` are gitignored. Do not commit them.

## Where the state lives

| File | Contents |
|---|---|
| `config.yaml` | app settings: `server`, `hue`/`shade`, global `terminal`, `local` card, `owner` |
| `servers.conf` | remote server inventory (`servers:` list), next to `config.yaml` |
| `auth.json` / `sessions.json` | password hash and session token hashes |
| `jobs.json` | detached tmux job cache (remote tmux is the source of truth) |
| `schedules.conf` | schedule definitions (next to `config.yaml`) |
| `schedules-state.json` | runtime schedule state: last runs, history, missed runs |
| `known_hosts` | TOFU-pinned SSH host keys |

`config.Servers` is `yaml:"-"`; never write server inventory into `config.yaml`.
`config.Load` reads both files and auto-migrates legacy `servers:` blocks from
`config.yaml` into `servers.conf`. In code, use `config.Save` (app settings),
`config.SaveServers` (inventory), or `config.SaveAll` (both).

## Security invariants — non-negotiable

These are the rules that must survive every change. The reasoning and the
audit trail live in `docs/security-audit.md`.

1. **Middleware order is load-bearing:** `SecurityHeaders → Guard → auth →
   debug → mux`. Guard stays *outside* auth (it covers public routes);
   debug stays *inside* auth. Do not reorder.
2. **Never log, audit, or echo the password value.** Passwords are bcrypt
   hashes in `auth.json`, never in `config.yaml`, never in logs.
3. **Session security:** tokens only ever stored as SHA-256 hashes in
   `sessions.json`; cookies are HttpOnly + SameSite=Strict; the login rate
   limiter stays. Physical file access implying access to the config dir is
   an accepted owner risk — do not "fix" by moving secrets into the cloud.
4. **SSH host keys are TOFU-pinned.** Do not reintroduce
   `InsecureIgnoreHostKey` as a default; the only opt-out is
   `server.insecure_ignore_host_key: true` (logged at startup). New dials go
   through `ssh.SetHostKeyPolicy`/`hostKeyCallback`.
5. **Data-derived markup never uses inline event handlers from data.** Server
   IDs are validated by `config.ValidateID`; remote values (container names,
   load, command output) are sanitized at the boundary and escaped client
   side with `esc()`. New markup sinks = new escaping work.
6. **All file writes go through `internal/storage`** — it owns state-dir
   resolution and permissions (`0700` dir, `0600` files, atomic
   temp+fsync+rename). Do not write state files with raw `os.WriteFile`.
7. **No CDN references in `web/`** — xterm.js is vendored in `web/vendor/`
   and embedded. The frontend stays a no-build-step vanilla JS file.
8. **No new runtime dependencies without a good reason.** Keep the single
   static binary. (Go test/dev deps: practically never needed.)
9. **Remote targets are Linux; bind-by-default loopback.** Never suggest
   `bind: "0.0.0.0"`; non-loopback binds require an encrypted overlay
   (Tailscale/WireGuard) and trigger the startup warning — by design.
10. **SSH commands are shell-quoted at the boundary** — `shellQuote()` in
    `handlers.go`/`jobs.go`; commands embedded in remote scripts use quoted
    heredocs/`sh -c`. Do not bypass quoting for user-supplied strings.

## Verification before handing work back

Run from `go/`:

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
CGO_ENABLED=0 go build ./...
```

Run `./dev` afterward if you changed runtime behavior. For concurrency work,
`./dev race` is available. Tests already cover the important behavior (see
`BUILD.md` → "Tests"); when fixing a bug, add the regression test at the same
place.

## Tooling reference

| Tool | What it does |
|---|---|
| `./dev` / `./dev run` / `./dev race` | Sandbox builds/runs; state in `test-config/`, port 7080, never touches the live install |
| `./build.sh` | Cross-compiles 8 platforms + macOS `.app`/DMGs → `dist/` |
| `./release.sh vX.Y.Z` | Full release: git checks → tests → build → checksums → notes validation → tag → GitHub publish (`--dry-run` for everything-but-publish) |
| `git` | Default branch is `main`; keep commits clean of `dist/`, `test-config/`, the binary |
| `gh` | Releases, repo metadata (authenticated as the owner) |

## Testing the UI with Playwright

Playwright is the way to exercise the browser UI. It is already available via
`npx` (version 1.64.1 at the time of writing); Chromium is cached locally.

### 1. Start the dev server

```sh
./dev run >/tmp/serverhub-dev.log 2>&1 &
pid=$!

# wait for it to be ready
for i in $(seq 1 40); do
  curl -sf http://127.0.0.1:7080/api/ping >/dev/null && break
  sleep 0.25
done
```

Read the URL from `./dev`'s output — in run mode it binds the device-
specific address set in the untracked `PRINFO.md` on this machine (gitignored;
never commit it), e.g. `http://100.X.Y.Z:7080`, not loopback. The Guard only
accepts the configured bind plus loopback names, and mutating requests need
either `Content-Type: application/json` or `X-Requested-With: XMLHttpRequest`.

Remember to stop it when done: `kill "$pid"`.

### 2. Quick visual check

```sh
npx playwright screenshot \
  --viewport-size=1280,800 \
  --wait-for-timeout=1200 \
  http://127.0.0.1:7080/ /tmp/serverhub-ui.png
```

This is the fastest way to confirm the page renders and to attach a screenshot
to a bug report.

### 3. Interactive runs

```sh
npx playwright codegen http://127.0.0.1:7080/
```

Use the inspector to walk the onboarding wizard, add a server, open a detail
page, etc., and copy the generated selectors into a test.

### 4. Repeatable tests (keep Node out of the repo)

The app deliberately has no Node dependency or JS build step. Put Playwright
tests in the gitignored `tools/` directory, never in `go/`:

```sh
mkdir -p tools/e2e && cd tools/e2e
npm init -y
npm i -D @playwright/test@1.64.1
npx playwright install chromium
```

Minimal `tools/e2e/smoke.spec.js`:

```js
const { test, expect } = require('@playwright/test');

test('login page renders without console errors', async ({ page }) => {
  const errors = [];
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  await page.goto('http://127.0.0.1:7080/');
  await expect(page).toHaveTitle(/ServerHub/);
  expect(errors).toEqual([]);
});
```

Run it while `./dev run` is up:

```sh
cd tools/e2e
npx playwright test smoke.spec.js --reporter=list
```

For multi-step flows, assert on visible text rather than CSS classes where
possible; the UI classes are implementation details and may change. Existing
spec files (`scheduler.spec.js`, `run-now.spec.js`) show the conventions —
including that the dev server binds a real IP, so read `SH_BASE` from env.

### Dev-state note for UI work

`./dev run` wipes state on every start, so each run begins at the onboarding
wizard. If you want state to persist across UI iterations, run `./dev` once
(build only) and then start `./serverhub` directly — the `devDir` override is
baked into that binary, so it still uses `test-config/`. Wipe with `./dev`
again when you want a clean slate.

## Releasing a version

When the user asks to "fix X and release" (or "release the next update"), the
fix and the release are one flow an agent can complete on its own:

1. **Fix the issue** and verify it (section above, plus the dev harness for
   runtime behavior).
2. **Write the release notes** at `docs/releases/vX.Y.Z.md` using the template
   in `docs/releases/README.md`: `## Added`, `## Changed`, `## Fixed` — one
   bullet per user-visible change since the previous tag; a section with
   nothing in it states "(none)". Pick the version by bumping the newest tag
   (`git tag -l 'v*'`): patch digit for fixes, minor digit for features.
3. **Commit and push** everything (the notes file included). This is allowed
   without asking when the user requested a release.
4. **Run `./release.sh vX.Y.Z`**. It enforces a clean, pushed-default-branch
   tree, runs vet + tests, builds all 8 platforms plus DMGs via `build.sh`,
   checksums the artifacts, validates the notes sections, tags `vX.Y.Z`, and
   publishes the GitHub release with binaries + `checksums.txt` attached.
5. Check the release URL it prints, and report it to the user.

Notes:
- `./release.sh vX.Y.Z --dry-run` builds and checks everything without
  touching tags or GitHub. Use it when unsure about the notes.
- If the script fails after tagging, **delete the tag** (`git tag -d vX.Y.Z`
  and `git push origin :vX.Y.Z`) before retrying. Never reuse or move a
  published tag.
- Never hand-edit `dist/` artifacts — `release.sh`/`build.sh` own them.
- If `gh` is not authenticated on the machine, `release.sh` fails at the
  GitHub step; tell the user rather than trying workarounds.

## Project conventions / gotchas

- **Do not act on `TODO.md`** unless the user explicitly asks; the file says so.
- `stress_test.sh` is dead weight and marked for removal.
- Data-derived markup: see the security invariants above — also the one rule
  most likely to fail review.
- Do not commit unless the user asks. Release flows are the exception, and
  only for the release commits described above. Inspect `git status`/`git diff`
  first, and keep `dist/`, `test-config/`, and the binary out of every commit.
- When you change behavior that users can see, update the matching doc
  (`README.md` for user-facing behavior, `BUILD.md` for architecture,
  `docs/CONFIGURATION.md` for config surface) in the same change.
- Keep the user's tone in mind: they want fixes, not feature creep. "I don't
  want it to do more, I just need it to do it better" is the standing rule.