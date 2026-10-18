#!/usr/bin/env bash
set -euo pipefail

# ── Config ────────────────────────────────────────────────────────────────────
APP="serverhub"
APP_DISPLAY="ServerHub"
BUNDLE_ID="com.serverhub.app"
VERSION="1.0.0"
LDFLAGS="-s -w -X main.version=${VERSION}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SRC_DIR="${SCRIPT_DIR}/go"
DIST_DIR="${SCRIPT_DIR}/dist"

# ── Args ──────────────────────────────────────────────────────────────────────
ICON_SRC="${SCRIPT_DIR}/resources/icon.png"
SKIP_DMG=false

usage() {
  echo "Usage: $(basename "$0") [options]"
  echo "  --icon <file.png>   Source PNG for icon conversion and app bundles"
  echo "  --version <semver>  Override version string (default: ${VERSION})"
  echo "  --no-dmg            Build .app bundles but skip DMG packaging"
  echo "  --help              Show this message"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --icon)    ICON_SRC="$(cd "$(dirname "$2")" && pwd)/$(basename "$2")"; shift 2 ;;
    --version) VERSION="$2"; LDFLAGS="-s -w -X main.version=${VERSION}"; shift 2 ;;
    --no-dmg)  SKIP_DMG=true; shift ;;
    --help|-h) usage ;;
    *) echo "Unknown option: $1"; exit 1 ;;
  esac
done

# ── Targets ───────────────────────────────────────────────────────────────────
TARGETS=(
  "darwin   amd64  "
  "darwin   arm64  "
  "linux    amd64  "
  "linux    arm64  "
  "linux    arm    7"   # ARMv7 — Raspberry Pi 2/3/4 (32-bit OS)
  "linux    arm    6"   # ARMv6 — Raspberry Pi 1 / Zero
  "windows  amd64  "
  "windows  arm64  "
)

# ── Colors ────────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'; RED='\033[0;31m'; CYAN='\033[0;36m'
DIM='\033[2m'; BOLD='\033[1m'; RESET='\033[0m'

info() { echo -e "  ${DIM}$*${RESET}"; }
step() { echo -e "  ${CYAN}▸${RESET} $*"; }
ok()   { echo -e "  ${GREEN}✓${RESET} $*"; }
err()  { echo -e "  ${RED}✗${RESET} $*"; }

human_size() {
  local bytes=$1
  if   (( bytes >= 1048576 )); then printf "%.1f MB" "$(echo "scale=1; $bytes/1048576" | bc)"
  elif (( bytes >= 1024    )); then printf "%.1f KB" "$(echo "scale=1; $bytes/1024"    | bc)"
  else printf "%d B" "$bytes"
  fi
}

# ── Icon: PNG → .icns (macOS) ─────────────────────────────────────────────────
# Uses sips + iconutil, both built into macOS. No external dependencies.
make_icns() {
  local src="$1" out_dir="$2"
  local iconset="${out_dir}/${APP}.iconset"
  mkdir -p "${iconset}"

  # Apple requires these specific filename/size pairs inside an .iconset folder
  sips -z 16   16   "${src}" --out "${iconset}/icon_16x16.png"      >/dev/null
  sips -z 32   32   "${src}" --out "${iconset}/icon_16x16@2x.png"   >/dev/null
  sips -z 32   32   "${src}" --out "${iconset}/icon_32x32.png"      >/dev/null
  sips -z 64   64   "${src}" --out "${iconset}/icon_32x32@2x.png"   >/dev/null
  sips -z 128  128  "${src}" --out "${iconset}/icon_128x128.png"    >/dev/null
  sips -z 256  256  "${src}" --out "${iconset}/icon_128x128@2x.png" >/dev/null
  sips -z 256  256  "${src}" --out "${iconset}/icon_256x256.png"    >/dev/null
  sips -z 512  512  "${src}" --out "${iconset}/icon_256x256@2x.png" >/dev/null
  sips -z 512  512  "${src}" --out "${iconset}/icon_512x512.png"    >/dev/null
  sips -z 1024 1024 "${src}" --out "${iconset}/icon_512x512@2x.png" >/dev/null

  iconutil -c icns "${iconset}" -o "${out_dir}/${APP}.icns"
  rm -rf "${iconset}"
  echo "${out_dir}/${APP}.icns"
}

# ── Icon: PNG → .ico (Windows) ────────────────────────────────────────────────
# Requires ImageMagick (`brew install imagemagick`). Skipped if not present.
make_ico() {
  local src="$1" out="$2"
  if ! command -v convert >/dev/null 2>&1; then
    echo ""; return
  fi
  convert "${src}" \
    \( -clone 0 -resize 16x16   \) \
    \( -clone 0 -resize 32x32   \) \
    \( -clone 0 -resize 48x48   \) \
    \( -clone 0 -resize 64x64   \) \
    \( -clone 0 -resize 128x128 \) \
    \( -clone 0 -resize 256x256 \) \
    -delete 0 "${out}" 2>/dev/null
  echo "${out}"
}

# ── macOS .app bundle ─────────────────────────────────────────────────────────
make_app_bundle() {
  local binary="$1" arch="$2" icns="$3" out_dir="$4"
  local bundle="${out_dir}/${APP_DISPLAY}.app"

  mkdir -p "${bundle}/Contents/MacOS"
  mkdir -p "${bundle}/Contents/Resources"

  cp "${binary}" "${bundle}/Contents/MacOS/${APP}"
  chmod +x "${bundle}/Contents/MacOS/${APP}"

  [[ -n "${icns}" && -f "${icns}" ]] && cp "${icns}" "${bundle}/Contents/Resources/${APP}.icns"

  cat > "${bundle}/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
    "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>         <string>${APP}</string>
  <key>CFBundleIdentifier</key>         <string>${BUNDLE_ID}</string>
  <key>CFBundleName</key>               <string>${APP_DISPLAY}</string>
  <key>CFBundleDisplayName</key>        <string>${APP_DISPLAY}</string>
  <key>CFBundleVersion</key>            <string>${VERSION}</string>
  <key>CFBundleShortVersionString</key> <string>${VERSION}</string>
  <key>CFBundlePackageType</key>        <string>APPL</string>
  <key>CFBundleSignature</key>          <string>????</string>
  <key>CFBundleIconFile</key>           <string>${APP}</string>
  <key>NSHighResolutionCapable</key>    <true/>
  <key>LSMinimumSystemVersion</key>     <string>11.0</string>
</dict>
</plist>
PLIST

  echo "${bundle}"
}

# ── DMG packaging ─────────────────────────────────────────────────────────────
# Uses only hdiutil, which ships with macOS. No brew dependencies.
make_dmg() {
  local bundle="$1" arch="$2" out_dir="$3"
  local vol="${APP_DISPLAY} ${VERSION}"
  local tmp_dmg="${out_dir}/.tmp-${APP}-${arch}.dmg"
  local dmg="${out_dir}/${APP}-${VERSION}-macos-${arch}.dmg"

  # Stamp a read-write image from the .app folder
  hdiutil create \
    -volname "${vol}" \
    -srcfolder "${bundle}" \
    -ov -format UDRW \
    "${tmp_dmg}" >/dev/null 2>&1

  # Mount it and drop in an /Applications symlink for drag-install UX
  local dev
  dev=$(hdiutil attach -readwrite -noverify -noautoopen "${tmp_dmg}" 2>/dev/null \
        | grep -E '^/dev/' | awk 'NR==1{print $1}')
  local mnt="/Volumes/${vol}"
  ln -sf /Applications "${mnt}/Applications" 2>/dev/null || true
  hdiutil detach "${dev}" >/dev/null 2>&1

  # Compress to a distributable read-only DMG
  hdiutil convert "${tmp_dmg}" -format UDZO -imagekey zlib-level=9 \
    -o "${dmg}" >/dev/null 2>&1
  rm -f "${tmp_dmg}"

  echo "${dmg}"
}

# ── Pre-flight ────────────────────────────────────────────────────────────────
ON_MACOS=false
[[ "$(uname)" == "Darwin" ]] && ON_MACOS=true

echo ""
echo -e "${BOLD}  ${APP_DISPLAY} ${VERSION} — cross-platform build${RESET}"
echo -e "  ${DIM}$(go version)${RESET}"
echo -e "  ${DIM}Source : ${SRC_DIR}${RESET}"
echo -e "  ${DIM}Output : ${DIST_DIR}${RESET}"
[[ -n "${ICON_SRC}" ]] && echo -e "  ${DIM}Icon   : ${ICON_SRC}${RESET}"
echo ""

# Validate icon path early
if [[ -n "${ICON_SRC}" && ! -f "${ICON_SRC}" ]]; then
  err "Icon file not found: ${ICON_SRC}"
  exit 1
fi

# Warn if icon was requested but we're not on macOS (sips/iconutil are macOS-only)
if [[ -n "${ICON_SRC}" && "${ON_MACOS}" == false ]]; then
  info "Icon conversion requires macOS (sips/iconutil). Skipping icon steps."
  ICON_SRC=""
fi

rm -rf "${DIST_DIR}"
mkdir -p "${DIST_DIR}"

# ── Icon conversion ───────────────────────────────────────────────────────────
ICNS_PATH=""
ICO_PATH=""

if [[ -n "${ICON_SRC}" ]]; then
  step "Converting icon..."
  ICON_DIR="${DIST_DIR}/.icons"
  mkdir -p "${ICON_DIR}"

  ICNS_PATH=$(make_icns "${ICON_SRC}" "${ICON_DIR}")
  ok ".icns → $(basename "${ICNS_PATH}")"

  ICO_PATH=$(make_ico "${ICON_SRC}" "${ICON_DIR}/${APP}.ico")
  if [[ -n "${ICO_PATH}" ]]; then
    ok ".ico  → $(basename "${ICO_PATH}")"
  else
    info "Skipping .ico — ImageMagick not found (brew install imagemagick)"
  fi

  # Copy a scaled copy of the source PNG alongside the other icons
  sips -z 1024 1024 "${ICON_SRC}" --out "${ICON_DIR}/${APP}-1024.png" >/dev/null
  ok ".png  → ${APP}-1024.png (1024×1024)"
  echo ""
fi

# ── Build loop ────────────────────────────────────────────────────────────────
declare -a RESULTS
declare -a DARWIN_BINS   # "arch|binary_path"
PASS=0; FAIL=0

for entry in "${TARGETS[@]}"; do
  read -r GOOS GOARCH GOARM <<< "$entry"

  name="${APP}-${GOOS}-${GOARCH}"
  [[ -n "${GOARM}" ]] && name="${name}v${GOARM}"
  [[ "${GOOS}" == "windows" ]] && name="${name}.exe"
  out="${DIST_DIR}/${name}"

  label="${GOOS}/${GOARCH}"
  [[ -n "${GOARM}" ]] && label="${label}v${GOARM}"

  printf "  %-26s" "${label}"

  env_vars="GOOS=${GOOS} GOARCH=${GOARCH} CGO_ENABLED=0"
  [[ -n "${GOARM}" ]] && env_vars="${env_vars} GOARM=${GOARM}"

  if eval "${env_vars} go build -C \"${SRC_DIR}\" -trimpath -ldflags=\"${LDFLAGS}\" -o \"${out}\" ." 2>/tmp/serverhub_build_err; then
    size=$(wc -c < "${out}")
    pretty=$(human_size "$size")
    echo -e "${GREEN}✓${RESET}  ${pretty}"
    RESULTS+=("${label}|${out}|${pretty}|ok")
    [[ "${GOOS}" == "darwin" && -z "${GOARM}" ]] && DARWIN_BINS+=("${GOARCH}|${out}")
    (( PASS++ )) || true
  else
    echo -e "${RED}✗  FAILED${RESET}"
    info "$(cat /tmp/serverhub_build_err)"
    RESULTS+=("${label}||—|fail")
    (( FAIL++ )) || true
  fi
done

# ── macOS .app bundles + DMGs ─────────────────────────────────────────────────
if [[ "${ON_MACOS}" == true && ${#DARWIN_BINS[@]} -gt 0 ]]; then
  echo ""
  step "Building macOS packages..."
  for entry in "${DARWIN_BINS[@]}"; do
    IFS='|' read -r arch bin <<< "$entry"
    bundle_dir="${DIST_DIR}/macos-${arch}"
    mkdir -p "${bundle_dir}"

    bundle=$(make_app_bundle "${bin}" "${arch}" "${ICNS_PATH}" "${bundle_dir}")
    ok "${APP_DISPLAY}.app  → dist/macos-${arch}/"

    if [[ "${SKIP_DMG}" == false ]]; then
      dmg=$(make_dmg "${bundle}" "${arch}" "${DIST_DIR}")
      ok "DMG          → dist/$(basename "${dmg}")"
    fi
  done
fi

# ── Summary ───────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}  Summary${RESET}"
echo "  ────────────────────────────────────────"
for r in "${RESULTS[@]}"; do
  IFS='|' read -r lbl _path size status <<< "$r"
  if [[ "${status}" == "ok" ]]; then
    printf "  ${GREEN}%-26s${RESET}  %s\n" "${lbl}" "${size}"
  else
    printf "  ${RED}%-26s  FAILED${RESET}\n" "${lbl}"
  fi
done
echo "  ────────────────────────────────────────"
echo -e "  ${BOLD}${PASS} built${RESET}  ${FAIL} failed"
echo ""

(( FAIL > 0 )) && exit 1

info "Artifacts in dist/"
echo ""
