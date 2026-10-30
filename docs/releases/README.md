# Release notes

Every release gets one committed notes file: `docs/releases/vX.Y.Z.md`.
`release.sh` validates it has all three sections and publishes it as the
GitHub release body.

## Template

```markdown
# ServerHub vX.Y.Z

Release notes for <one-line summary if wanted>.

## Added

- <new things, e.g. "Per-schedule timeout field">

## Changed

- <behaviour or UI changes, e.g. "Nightly intervals keep clock time across DST">

## Fixed

- <bug fixes, e.g. "Cron did not fire twice in fall-back hour">

```

## Rules

- Always include all three sections. If a section is empty, write one line:
  its heading followed by "(none)" — e.g.

  ```
  ## Added

  (none)
  ```
- One bullet per user-visible change, in plain language a server admin
  understands. Internal refactors only get a bullet if they change behaviour.
- Tag names are `vX.Y.Z`; bump the patch digit for fixes, the minor digit for
  new features. Check the previous tag: `git tag -l 'v*'`.
- The notes file must be committed and pushed before running `./release.sh`.