#!/usr/bin/env bash
set -euo pipefail

# ── ServerHub release script ─────────────────────────────────────────────────
# Verifies the tree is clean and in sync, runs the test suite, builds every
# platform binary via build.sh, validates the release notes, tags, and
# publishes a GitHub release with all artifacts.
#
# Usage:
#   ./release.sh vX.Y.Z                                 # full release
#   ./release.sh vX.Y.Z --notes docs/releases/vX.Y.Z.md # explicit notes file
#   ./release.sh vX.Y.Z --dry-run                       # build + checks only
#   ./release.sh vX.Y.Z --draft                         # create GH release as draft
#
# Release notes MUST contain Added / Changed / Fixed sections (see
# docs/releases/README.md for the template). The notes file must be committed
# before the release: this script refuses to run on a dirty tree.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

VERSION=""
NOTES=""
DRY_RUN=false
DRAFT=false

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,2\}//'
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --notes)     NOTES="$2"; shift 2 ;;
    --dry-run)   DRY_RUN=true; shift ;;
    --draft)     DRAFT=true; shift ;;
    --help|-h)   usage ;;
    -*)          echo "Unknown option: $1"; usage ;;
    *)           VERSION="$1"; shift ;;
  esac
done

# ── Colors ────────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'
DIM='\033[2m'; BOLD='\033[1m'; RESET='\033[0m'
step() { echo -e "  ${CYAN}▸${RESET} $*"; }
ok()   { echo -e "  ${GREEN}✓${RESET} $*"; }
err()  { echo -e "  ${RED}✗${RESET} $*"; }
die()  { err "$*"; exit 1; }

[[ -n "${VERSION}" ]] || die "usage: ./release.sh vX.Y.Z [--notes FILE] [--dry-run]"

# Normalise the version: accept X.Y.Z or vX.Y.Z, always tag vX.Y.Z.
VERSION="${VERSION#v}"
[[ "${VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "version must be semver (X.Y.Z), got: ${VERSION}"
TAG="v${VERSION}"

echo ""
echo -e "${BOLD}  ServerHub ${TAG} — release${RESET}"
echo ""

# ── 1. Git preconditions ─────────────────────────────────────────────────────
step "Checking git state..."

command -v git >/dev/null || die "git not found"

# Clean tree: staged or modified tracked changes are not allowed (untracked
# files are the developer's local business and never end up in a release).
if [[ -n "$(git status --porcelain | grep -v '^??' || true)" ]]; then
  git status --short | head -10
  die "working tree has uncommitted tracked changes — commit them first"
fi
ok "working tree clean"

# The branch must have an upstream and be in sync, both ways.
git fetch --quiet origin 2>/dev/null || true
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
if ! git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' >/dev/null 2>&1; then
  die "branch '${BRANCH}' has no upstream — push it first (git push -u origin ${BRANCH})"
fi
UPSTREAM="$(git rev-parse --abbrev-ref --symbolic-full-name '@{upstream}')"
UNPUSHED="$(git rev-list --count "@{upstream}..HEAD")"
[[ "${UNPUSHED}" -eq 0 ]] || die "${UNPUSHED} unpushed commit(s) — push first (git push)"
UNPULLED="$(git rev-list --count "HEAD..@{upstream}")"
[[ "${UNPULLED}" -eq 0 ]] || die "missing ${UNPULLED} upstream commit(s) — pull first (git pull --ff-only)"
ok "branch '${BRANCH}' in sync with ${UPSTREAM}"

# Tagging the default branch keeps releases from stray local branches.
DEF_BRANCH="$(git symbolic-ref --short refs/remotes/origin/HEAD 2>/dev/null | sed 's|^origin/||' || true)"
if [[ -n "${DEF_BRANCH}" && "${BRANCH}" != "${DEF_BRANCH}" ]]; then
  die "releases are made from ${DEF_BRANCH} — current branch is ${BRANCH}"
fi
[[ -n "${DEF_BRANCH}" ]] && ok "on default branch (${DEF_BRANCH})"

if git rev-parse -q --verify "refs/tags/${TAG}" >/dev/null; then
  die "tag ${TAG} already exists — pick the next version"
fi

# ── 2. Release notes ─────────────────────────────────────────────────────────
if [[ -z "${NOTES}" ]]; then
  NOTES="docs/releases/${TAG}.md"
fi
[[ -f "${NOTES}" ]] || die "release notes not found: ${NOTES}
Write them at ${NOTES} with Added / Changed / Fixed sections
(see docs/releases/README.md), commit, push, then re-run."

for section in Added Changed Fixed; do
  if ! grep -Eiq "^#+[[:space:]]+${section}\b" "${NOTES}"; then
    die "release notes missing a '${section}' section (${NOTES})"
  fi
done
ok "release notes at ${NOTES} (Added/Changed/Fixed present)"

# ── 3. Tests ─────────────────────────────────────────────────────────────────
step "Running tests..."
if ! ( cd go && CGO_ENABLED=0 go vet ./... && go test ./... ) >/tmp/serverhub_release_tests 2>&1; then
  tail -20 /tmp/serverhub_release_tests
  die "tests failed — release aborted"
fi
ok "vet + tests pass"

# ── 4. Build all platforms ───────────────────────────────────────────────────
step "Building all platforms (${VERSION})..."
./build.sh --version "${VERSION}" || die "build failed"

step "Checksumming artifacts..."
HAVE_SHASUM=false
command -v shasum >/dev/null 2>&1 && HAVE_SHASUM=true
ARTIFACTS=()
for f in dist/serverhub-* dist/*.dmg; do
  [[ -f "$f" ]] && ARTIFACTS+=("$f")
done
[[ ${#ARTIFACTS[@]} -gt 0 ]] || die "no artifacts found in dist/"
CHECKSUMS="dist/checksums.txt"
: > "${CHECKSUMS}"
for f in "${ARTIFACTS[@]}"; do
  if [[ "${HAVE_SHASUM}" == true ]]; then
    ( cd dist && shasum -a 256 "$(basename "$f")" ) >> "${CHECKSUMS}"
  else
    ( cd dist && sha256sum  "$(basename "$f")" ) >> "${CHECKSUMS}"
  fi
done
ok "${#ARTIFACTS[@]} artifacts + dist/checksums.txt"

# ── 5. Tag, push, publish ────────────────────────────────────────────────────
if [[ "${DRY_RUN}" == true ]]; then
  echo ""
  ok "dry run — built, checksummed, NOT tagged or published"
  echo -e "  ${DIM}notes would come from: ${NOTES}${RESET}"
  exit 0
fi

step "Tagging ${TAG}..."
git tag -a "${TAG}" -m "ServerHub ${TAG}" && ok "tagged"
step "Pushing ${TAG}..."
git push origin "${TAG}" && ok "pushed ${TAG}"

step "Creating GitHub release..."
GH_ARGS=( "dist/checksums.txt" )
GH_ARGS+=( "${ARTIFACTS[@]}" )
if [[ "${DRAFT}" == true ]]; then
  gh release create "${TAG}" "${GH_ARGS[@]}" \
    --title "ServerHub ${TAG}" \
    --notes-file "${NOTES}" \
    --draft
  ok "draft release created — review and publish on GitHub"
else
  gh release create "${TAG}" "${GH_ARGS[@]}" \
    --title "ServerHub ${TAG}" \
    --notes-file "${NOTES}"
  ok "release published: https://github.com/$(git config --get remote.origin.url | sed -E 's#.*(github\.com/)(.*)\.git$#\2#')/releases/tag/${TAG}"
fi

echo ""
echo -e "${BOLD}  Release ${TAG} done.${RESET}"