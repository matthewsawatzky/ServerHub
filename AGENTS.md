# AGENTS.md — ServerHub

Read this before touching the codebase. It is the short, practical version of
`BUILD.md`; the full architecture and design rationale live there.

## Hard rule: never test against the live install

The live instance uses the OS config directory (`~/Library/Application Support/ServerHub`,
`~/.config/ServerHub`, `%AppData%\ServerHub`) and is **off-limits** while this
version is in beta.

Use the dev harness for every build and test:

```sh
./dev            # build for this machine, state sandboxed in ./test-config/
./dev run        # build, then run on http://127.0.0.1:7080
./dev race       # build with the Go race detector
```

`./dev` deletes the previous dev binary and **wipes `test-config/`** on every
run, then bakes `-X serverhub/internal/storage.devDir=<repo>/test-config` into
the binary. That override redirects *all* state — `config.yaml`, `servers.conf`,
`auth.json`, `sessions.json`, `jobs.json`, `schedules.conf`,
`schedules-state.json`, `known_hosts` — into `test-config/`. The dev
config listens on port **7080** (live default is 7070) and does not open a
browser.

Important: passing `./serverhub /some/other/config.yaml` does **not** redirect
auth/session/host-key state. Unless the binary was built with the `storage.devDir`
ldflag above, those files still resolve to the live OS config directory. When in
doubt, use `./dev`.

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

## Verification before handing work back

Run from `go/`:

```sh
gofmt -l .          # must print nothing
go vet ./...
go test ./...
CGO_ENABLED=0 go build ./...
```

Run `./dev` afterward if you changed runtime behavior. For concurrency work,
`./dev race` is available.

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

Use `http://127.0.0.1:7080` (not a hostname). The Guard only accepts the
configured bind plus loopback names, and mutating requests need either
`Content-Type: application/json` or `X-Requested-With: XMLHttpRequest`.

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
possible; the UI classes are implementation details and may change.

### Dev-state note for UI work

`./dev run` wipes state on every start, so each run begins at the onboarding
wizard. If you want state to persist across UI iterations, run `./dev` once
(build only) and then start `./serverhub` directly — the `devDir` override is
baked into that binary, so it still uses `test-config/`. Wipe with `./dev`
again when you want a clean slate.

## Project conventions / gotchas

- Read `BUILD.md` for architecture; `docs/security-audit.md` explains the threat
  model and which mitigations are deliberate. `docs/design-jobs-scheduler.md` is the
  agreed model for future SSH jobs, detached quick actions, schedules, and
  probes — do not diverge from it without discussing the change.
- **Do not act on `TODO.md`** unless the user explicitly asks; the file says so.
- `stress_test.sh` is dead weight and marked for removal.
- No new runtime dependencies without a good reason; keep the single static
  binary and the no-build-step frontend.
- Data-derived markup must never use inline handlers built from data. Server IDs
  are validated by `config.ValidateID`; remote container names/load values are
  sanitized at the boundary and escaped client-side.
- Middleware order is load-bearing:
  `SecurityHeaders → Guard → auth → debug → mux`.
- Never log or audit the password value.
- Do not commit unless the user asks. When you do, inspect `git status`/`git diff`
  first and keep `dist/`, `test-config/`, and the binary out of the commit.
