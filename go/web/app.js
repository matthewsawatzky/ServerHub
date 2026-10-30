'use strict';

// ── Full theme definitions ────────────────────────────────────────────────────

// ── Color config (hue + shade sliders) ───────────────────────────────────────

let accentHue = 120;
let backgroundColor = '#808080';
let backgroundColorEnabled = true;
let backgroundShade = 20;
let surfaceColor = '#808080';
let surfaceColorEnabled = true;
let surfaceShade = 23;
let animationsEnabled = true;
let dashboardOrder = ['apps', 'favorites', 'quick_actions'];
let dashboardShowDays = true;
let dashboardHidden = [];
let sizeUnits = 'GiB';
let dashboardDragId = null;

// Web-terminal appearance. Mirrored by terminal.html via /api/appconfig.
const TERMINAL_DEFAULTS = Object.freeze({
  background: '#0e0e10',
  foreground: '#e0e0e8',
  accent: '#639922',
});
let terminalConfig = { ...TERMINAL_DEFAULTS };
const _termInputSuffix = { background: 'bg', foreground: 'fg', accent: 'accent' };

// ── Palette maths ────────────────────────────────────────────────────────────
//
// Greys are placed in WCAG space instead of lerping HSL lightness: each tone
// is chosen by the contrast ratio it must reach against the grey it sits on,
// so the whole 0–100 shade range stays readable. Plain arithmetic — every
// custom property still ends up as a simple hsl() string.
//
// Mirrored in login.html for the wizard — keep the two in sync.

// sRGB ↔ linear light and the WCAG contrast ratio between two luminances.
function _lin(v) { return v <= 0.04045 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4); }
function _srgb(y) { return y <= 0.0031308 ? y * 12.92 : 1.055 * Math.pow(y, 1 / 2.4) - 0.055; }
function _lum(r, g, b) { return 0.2126 * _lin(r) + 0.7152 * _lin(g) + 0.0722 * _lin(b); }
function _contrast(y1, y2) {
  return (Math.max(y1, y2) + 0.05) / (Math.min(y1, y2) + 0.05);
}

// hsl(0,0%,l%) ↔ luminance, and the grey lightness that reaches `target`
// contrast against a surface (light text on dark halves, dark text on light).
function _greyY(l) { return _lin(l / 100); }
function _greyL(y) { return _srgb(Math.max(0, Math.min(1, y))) * 100; }
function _tierL(y, target, dark) {
  return _greyL(dark ? target * (y + 0.05) - 0.05 : (y + 0.05) / target - 0.05);
}

// The grey where dark and light text have identical contrast. Polarity flips
// here (≈46.03), not at 50: above this point light text can no longer reach
// 4.5:1, so the seam sits at the last shade where both sides clear AA.
const SEAM = _greyL(Math.sqrt(1.05 * 0.05) - 0.05);

// Fixed label tones placed on accent surfaces.
const ACCENT_LABEL_DARK = '#101014', ACCENT_LABEL_LIGHT = '#f4f4f6';
const _LABEL_DARK_Y  = _lum(0x10 / 255, 0x10 / 255, 0x14 / 255);
const _LABEL_LIGHT_Y = _lum(0xf4 / 255, 0xf4 / 255, 0xf6 / 255);

function _accentRgb(hue, l) {
  const c = 0.70 * (1 - Math.abs(2 * l / 100 - 1));
  const hp = ((hue % 360) + 360) % 360 / 60;
  const x = c * (1 - Math.abs(hp % 2 - 1));
  let r = 0, g = 0, b = 0;
  if      (hp < 1) { r = c; g = x; }
  else if (hp < 2) { r = x; g = c; }
  else if (hp < 3) { g = c; b = x; }
  else if (hp < 4) { g = x; b = c; }
  else if (hp < 5) { r = x; b = c; }
  else             { r = c; b = x; }
  const m = l / 100 - c / 2;
  return [r + m, g + m, b + m];
}

function _accentLabelContrast(hue, l) {
  const [r, g, b] = _accentRgb(hue, l);
  const y = _lum(r, g, b);
  return Math.max(_contrast(y, _LABEL_DARK_Y), _contrast(y, _LABEL_LIGHT_Y));
}

// accentLightness adapts the accent to the background half: 55 keeps accents
// vivid on dark shades; on light shades 30 is needed or accent-as-text
// (brand, active nav, badges) sits at ~1:1 on white. A few hues straddle the
// luminance where neither label tone reaches 4.5:1, so step ±1 — lighter on
// dark themes, darker on light — until the better tone clears AA (≤3 steps).
function accentLightness(hue, bg) {
  const dark = bg <= SEAM, base = dark ? 55 : 30, dir = dark ? 1 : -1;
  for (let d = 0; d <= 6; d++) {
    for (let k = 0; k < (d ? 2 : 1); k++) {
      const l = base + dir * d * (k ? -1 : 1);
      if (l >= 24 && l <= 70 && _accentLabelContrast(hue, l) >= 4.5) return l;
    }
  }
  return base;
}

// accentTextColor picks the fixed label tone with the higher WCAG contrast —
// always ≥4.5:1 once accentLightness() has cleared the degenerate hues.
function accentTextColor(hue, l) {
  const [r, g, b] = _accentRgb(hue, l);
  const y = _lum(r, g, b);
  return _contrast(y, _LABEL_DARK_Y) >= _contrast(y, _LABEL_LIGHT_Y)
    ? ACCENT_LABEL_DARK : ACCENT_LABEL_LIGHT;
}

// shadeSurfaceVars maps the shade slider (0=black … 100=white) onto the whole
// grey ramp. Surfaces hug the background (offsets scaled by 0.4–1.0, never
// below 40%) so cards and borders stay visible at every shade. Text tiers are
// placed by target contrast against the layered surface they sit on: primary
// ≥4.5:1 (9–17:1 in practice), muted ≥4.5:1, dim ≥3:1. Targets carry +0.05
// slack so rounding the lightness to 0.1 can't undercut AA.
function shadeSurfaceVars(s, set) {
  const bg = Math.max(2, Math.min(100, s));
  const dark = bg <= SEAM;
  const t = dark ? 1 - bg / SEAM : (bg - SEAM) / (100 - SEAM);
  const mag = 0.4 + 0.6 * t;             // hug magnitude: 40% → 100%
  const sign = dark ? 1 : -1;            // dark: layers lighter, light: darker
  const hug = (x) => sign * mag * x;
  const pc = (l) => Math.round(l * 10) / 10;

  const surface  = bg + hug(3);
  const surface2 = bg + hug(6);
  const surface3 = bg + hug(9);
  const border   = bg + hug(12);
  const textL    = dark ? Math.max(91, _tierL(_greyY(surface2), 4.55, true))
                        : Math.min(9,  _tierL(_greyY(surface2), 4.55, false));
  const mutedL   = _tierL(_greyY(surface), 4.55, dark);
  const dimL     = _tierL(_greyY(surface), 3.05, dark);

  set('--bg',         `hsl(0,0%,${pc(bg)}%)`);
  set('--surface',    `hsl(0,0%,${pc(surface)}%)`);
  set('--surface2',   `hsl(0,0%,${pc(surface2)}%)`);
  set('--surface3',   `hsl(0,0%,${pc(surface3)}%)`);
  set('--border',     `hsl(0,0%,${pc(border)}%)`);
  set('--text',       `hsl(0,0%,${pc(textL)}%)`);
  set('--text-muted', `hsl(0,0%,${pc(mutedL)}%)`);
  set('--text-dim',   `hsl(0,0%,${pc(dimL)}%)`);
  set('--offline',    `hsl(0,0%,${pc(dimL)}%)`);
}

function _clamp(n, min, max, fallback) {
  n = Number(n);
  if (!Number.isFinite(n)) return fallback;
  return Math.min(max, Math.max(min, n));
}

function _defaultSurfaceShade(bg) {
  return bg <= SEAM ? Math.min(100, bg + 3) : Math.max(2, bg - 3);
}

function _hexToHsl(hex) {
  const v = _normHex(hex) || '#808080';
  const r = parseInt(v.slice(1, 3), 16) / 255;
  const g = parseInt(v.slice(3, 5), 16) / 255;
  const b = parseInt(v.slice(5, 7), 16) / 255;
  const max = Math.max(r, g, b), min = Math.min(r, g, b);
  const l = (max + min) / 2;
  const d = max - min;
  if (!d) return { hue: 0, sat: 0 };
  const sat = d / (1 - Math.abs(2 * l - 1));
  let hue;
  if (max === r) hue = ((g - b) / d) % 6;
  else if (max === g) hue = (b - r) / d + 2;
  else hue = (r - g) / d + 4;
  hue = Math.round(hue * 60);
  if (hue < 0) hue += 360;
  return { hue, sat: Math.round(sat * 100) };
}

function _hslToHex(hue, sat, light) {
  hue = ((hue % 360) + 360) % 360;
  sat /= 100;
  light /= 100;
  const c = (1 - Math.abs(2 * light - 1)) * sat;
  const x = c * (1 - Math.abs((hue / 60) % 2 - 1));
  const m = light - c / 2;
  let r = 0, g = 0, b = 0;
  if (hue < 60) { r = c; g = x; }
  else if (hue < 120) { r = x; g = c; }
  else if (hue < 180) { g = c; b = x; }
  else if (hue < 240) { g = x; b = c; }
  else if (hue < 300) { r = x; b = c; }
  else { r = c; b = x; }
  const to = v => Math.round((v + m) * 255).toString(16).padStart(2, '0');
  return '#' + to(r) + to(g) + to(b);
}

function applyThemeConfig(cfg = {}) {
  accentHue = _clamp(cfg.hue ?? 120, 0, 360, 120);
  backgroundColor = cfg.background_color || '#808080';
  backgroundColorEnabled = cfg.background_color_enabled !== false;
  backgroundShade = _clamp(cfg.shade ?? 20, 0, 100, 20);
  surfaceColor = cfg.surface_color || '#808080';
  surfaceColorEnabled = cfg.surface_color_enabled !== false;
  surfaceShade = _clamp(
    cfg.surface_shade ?? _defaultSurfaceShade(backgroundShade),
    0, 100, _defaultSurfaceShade(backgroundShade)
  );
  applyTheme();
}

function applyTheme() {
  const set = (name, val) => document.documentElement.style.setProperty(name, val);
  const bg = backgroundShade;
  const accentL = accentLightness(accentHue, bg);

  set('--accent', `hsl(${accentHue},70%,${accentL}%)`);
  set('--accent-dim', `hsla(${accentHue},70%,${accentL}%,0.15)`);
  set('--accent-text', accentTextColor(accentHue, accentL));
  set('--acceptance-green', '#4caf50');
  set('--green', '#4caf50');
  set('--amber', '#ffb300');
  set('--red', '#e53935');
  set('--overlay', 'rgba(0,0,0,0.60)');
  set('--shadow', 'rgba(0,0,0,0.45)');
  set('--font-ui', "system-ui,-apple-system,'Segoe UI',sans-serif");
  set('--font-mono', "'Cascadia Code','Fira Code','Consolas','Menlo',monospace");
  const bgHsl = backgroundColorEnabled ? _hexToHsl(backgroundColor) : { hue: 0, sat: 0 };
  set('--bg', `hsl(${bgHsl.hue},${bgHsl.sat}%,${Math.round(bg * 10) / 10}%)`);

  const s = surfaceShade;
  const surfHsl = surfaceColorEnabled ? _hexToHsl(surfaceColor) : { hue: 0, sat: 0 };
  const dark = s <= SEAM;
  const t = dark ? 1 - s / SEAM : (s - SEAM) / (100 - SEAM);
  const mag = 0.4 + 0.6 * t;
  const sign = dark ? 1 : -1;
  const pc = l => Math.round(l * 10) / 10;
  const surf2 = s + sign * mag * 3;
  const surf3 = s + sign * mag * 6;
  const border = s + sign * mag * 9;
  const textL = dark ? Math.max(91, _tierL(_greyY(surf2), 4.55, true))
                     : Math.min(9, _tierL(_greyY(surf2), 4.55, false));
  const mutedL = _tierL(_greyY(s), 4.55, dark);
  const dimL = _tierL(_greyY(s), 3.05, dark);

  set('--surface', `hsl(${surfHsl.hue},${surfHsl.sat}%,${pc(s)}%)`);
  set('--surface2', `hsl(${surfHsl.hue},${surfHsl.sat}%,${pc(surf2)}%)`);
  set('--surface3', `hsl(${surfHsl.hue},${surfHsl.sat}%,${pc(surf3)}%)`);
  set('--border', `hsl(${surfHsl.hue},${surfHsl.sat}%,${pc(border)}%)`);
  set('--text', `hsl(0,0%,${pc(textL)}%)`);
  set('--text-muted', `hsl(0,0%,${pc(mutedL)}%)`);
  set('--text-dim', `hsl(0,0%,${pc(dimL)}%)`);
  set('--offline', `hsl(0,0%,${pc(dimL)}%)`);

  const hueEl = document.getElementById('hue-slider');
  if (hueEl) hueEl.value = accentHue;
  const bgHueEl = document.getElementById('bg-hue-slider');
  if (bgHueEl) { bgHueEl.value = bgHsl.hue; bgHueEl.disabled = !backgroundColorEnabled; }
  const bgEnabledEl = document.getElementById('bg-color-enabled');
  if (bgEnabledEl) bgEnabledEl.checked = backgroundColorEnabled;
  const shadeEl = document.getElementById('shade-slider');
  if (shadeEl) shadeEl.value = backgroundShade;
  const surfHueEl = document.getElementById('surface-hue-slider');
  if (surfHueEl) { surfHueEl.value = surfHsl.hue; surfHueEl.disabled = !surfaceColorEnabled; }
  const surfEnabledEl = document.getElementById('surface-color-enabled');
  if (surfEnabledEl) surfEnabledEl.checked = surfaceColorEnabled;
  const surfShadeEl = document.getElementById('surface-shade-slider');
  if (surfShadeEl) surfShadeEl.value = surfaceShade;
}

function setColorEnabled(kind, enabled) {
  if (kind === 'background') backgroundColorEnabled = !!enabled;
  else surfaceColorEnabled = !!enabled;
  themeChanged();
}

function hueSliderChanged(kind) {
  if (kind === 'background' && !backgroundColorEnabled) return;
  if (kind === 'surface' && !surfaceColorEnabled) return;
  const id = kind === 'background' ? 'bg-hue-slider' : 'surface-hue-slider';
  const el = document.getElementById(id);
  if (!el) return;
  const hue = +el.value;
  const current = kind === 'background' ? backgroundColor : surfaceColor;
  const hsl = _hexToHsl(current);
  const sat = hsl.sat < 1 ? 35 : hsl.sat;
  const hex = _hslToHex(hue, sat, 50);
  if (kind === 'background') backgroundColor = hex;
  else surfaceColor = hex;
  themeChanged();
}

function themeChanged() {
  const hueEl = document.getElementById('hue-slider');
  const shadeEl = document.getElementById('shade-slider');
  const surfShadeEl = document.getElementById('surface-shade-slider');
  applyThemeConfig({
    hue: hueEl ? +hueEl.value : accentHue,
    background_color: backgroundColor,
    background_color_enabled: backgroundColorEnabled,
    shade: shadeEl ? +shadeEl.value : backgroundShade,
    surface_color: surfaceColor,
    surface_color_enabled: surfaceColorEnabled,
    surface_shade: surfShadeEl ? +surfShadeEl.value : surfaceShade,
  });
  _saveAppConfig();
}

function _normalizeDashboardOrder(order) {
  const allowed = ['apps', 'favorites', 'quick_actions'];
  const out = [];
  for (const id of order || []) {
    if (allowed.includes(id) && !out.includes(id)) out.push(id);
  }
  for (const id of allowed) if (!out.includes(id)) out.push(id);
  return out;
}

function _normalizeDashboardHidden(hidden) {
  const allowed = ['apps', 'favorites', 'quick_actions'];
  const out = [];
  for (const id of hidden || []) {
    if (allowed.includes(id) && !out.includes(id)) out.push(id);
  }
  return out;
}

function applyDashboardConfig(order, days, hidden) {
  dashboardOrder = _normalizeDashboardOrder(order);
  dashboardShowDays = days !== false;
  dashboardHidden = _normalizeDashboardHidden(hidden);
  const week = document.querySelector('.dashboard-week-wrap');
  if (week) week.style.display = dashboardShowDays ? '' : 'none';
  const daysToggle = document.getElementById('dash-days-toggle');
  if (daysToggle) daysToggle.checked = dashboardShowDays;
  const view = document.getElementById('view-dashboard');
  if (view) {
    const ids = {
      apps: 'dashboard-services-section',
      favorites: 'favorites-section',
      quick_actions: 'dashboard-qa-section',
    };
    for (const id of dashboardOrder) {
      const el = document.getElementById(ids[id]);
      if (!el) continue;
      el.style.display = dashboardHidden.includes(id) ? 'none' : '';
      view.appendChild(el);
    }
  }
  renderDashboardOrderSettings();
}

function renderDashboardOrderSettings() {
  const list = document.getElementById('dashboard-order-list');
  if (!list) return;
  const labels = { apps: 'Apps', favorites: 'Favorites', quick_actions: 'Quick actions' };
  list.innerHTML = dashboardOrder.map((id, i) => `
    <div class="dash-order-row" draggable="true" data-dash-order="${esc(id)}">
      <span class="dash-order-handle">⋮⋮</span>
      <span class="dash-order-label">${esc(labels[id] || id)}</span>
      <label class="ui-toggle dash-order-show" title="Show on dashboard">
        <input type="checkbox" data-dash-show="${esc(id)}" ${dashboardHidden.includes(id) ? '' : 'checked'}>
        <span class="ui-toggle-box"></span>
      </label>
      <span class="dash-order-buttons">
        <button type="button" class="btn btn-ghost btn-sm" data-dash-move="-1" ${i === 0 ? 'disabled' : ''}>↑</button>
        <button type="button" class="btn btn-ghost btn-sm" data-dash-move="1" ${i === dashboardOrder.length - 1 ? 'disabled' : ''}>↓</button>
      </span>
    </div>`).join('');
}

function moveDashboardSection(id, delta) {
  const idx = dashboardOrder.indexOf(id);
  const to = idx + delta;
  if (idx === -1 || to < 0 || to >= dashboardOrder.length) return;
  [dashboardOrder[idx], dashboardOrder[to]] = [dashboardOrder[to], dashboardOrder[idx]];
  applyDashboardConfig(dashboardOrder, dashboardShowDays, dashboardHidden);
  _saveAppConfig();
}

function setDashboardSectionVisible(id, visible) {
  if (visible) dashboardHidden = dashboardHidden.filter(x => x !== id);
  else if (!dashboardHidden.includes(id)) dashboardHidden.push(id);
  applyDashboardConfig(dashboardOrder, dashboardShowDays, dashboardHidden);
  _saveAppConfig();
}

function setDashboardDays(enabled) {
  applyDashboardConfig(dashboardOrder, enabled, dashboardHidden);
  _saveAppConfig();
}

function applyAnimations(enabled) {
  animationsEnabled = enabled;
  document.documentElement.classList.toggle('no-anim', !enabled);
  const cb = document.getElementById('anim-toggle');
  if (cb) cb.checked = enabled;
}

// ── Background image ─────────────────────────────────────────────────────────

let backgroundImage = '';
let backgroundBlur = 12;
let _bgSaveTimer = null;

function applyBackgroundConfig(image, blur) {
  backgroundImage = image || '';
  backgroundBlur = Math.max(0, Math.min(60, Number(blur) || 0));
  const layer = document.getElementById('main-bg');
  if (layer) {
    if (backgroundImage) {
      layer.style.backgroundImage = `url('/api/background?v=${encodeURIComponent(backgroundImage)}')`;
      layer.style.setProperty('--bg-blur', backgroundBlur + 'px');
      layer.classList.add('has-image');
    } else {
      layer.classList.remove('has-image');
      layer.style.backgroundImage = '';
    }
  }
  const slider = document.getElementById('bg-blur-slider');
  if (slider) slider.value = backgroundBlur;
  const value = document.getElementById('bg-blur-value');
  if (value) value.textContent = backgroundBlur + 'px';
  const remove = document.getElementById('bg-remove');
  if (remove) remove.style.display = backgroundImage ? '' : 'none';
  const current = document.getElementById('bg-current');
  if (current) current.textContent = backgroundImage || 'No image selected';
}

function bgMsg(text, cls) {
  const el = document.getElementById('bg-msg');
  if (!el) return;
  el.textContent = text || '';
  el.className = 'pw-msg' + (cls ? ' ' + cls : '');
}

function onBackgroundBlur(value) {
  applyBackgroundConfig(backgroundImage, value);
  clearTimeout(_bgSaveTimer);
  _bgSaveTimer = setTimeout(async () => {
    try {
      await fetch('/api/appconfig', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'XMLHttpRequest' },
        body: JSON.stringify({ background_blur: backgroundBlur }),
      });
    } catch (_) {}
  }, 400);
}

let mediaLibraryKind = 'all';
let mediaLibraryItems = [];
let mediaLibraryOnSelect = null;

async function syncBackgroundConfig() {
  try {
    const cfg = await apiFetch('/api/appconfig');
    const blur = cfg.background_image ? (cfg.background_blur ?? 12) : (cfg.background_blur || 12);
    applyBackgroundConfig(cfg.background_image || '', blur);
    return cfg;
  } catch (_) {
    return null;
  }
}

function openBackgroundLibrary() {
  openMediaLibrary({
    kind: 'background',
    title: 'Choose background',
    onSelect: async item => {
      await apiFetch('/api/appconfig', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ background_image: item.name }),
      });
      applyBackgroundConfig(item.name, backgroundBlur);
      bgMsg('Background selected', 'ok');
      toast('Background selected', 'ok');
    },
  });
}

function openMediaLibrary(opts = {}) {
  mediaLibraryKind = opts.kind || 'all';
  mediaLibraryOnSelect = opts.onSelect || null;
  document.getElementById('media-modal-title').textContent = opts.title || 'Image library';
  document.getElementById('media-msg').textContent = '';
  setMediaFilter(mediaLibraryKind, false);
  const fileInput = document.getElementById('media-file');
  if (fileInput) fileInput.value = '';
  const fileLabel = document.getElementById('media-file-label');
  if (fileLabel) fileLabel.dataset.file = '';
  document.getElementById('media-url').value = '';
  document.getElementById('media-modal').style.display = 'flex';
  document.addEventListener('keydown', mediaKeyHandler);
  loadMediaLibrary();
}

function closeMediaLibrary() {
  document.getElementById('media-modal').style.display = 'none';
  document.removeEventListener('keydown', mediaKeyHandler);
  mediaLibraryOnSelect = null;
}

function mediaKeyHandler(e) {
  if (e.key === 'Escape') closeMediaLibrary();
}

function mediaOverlayClick(e) {
  if (e.target === document.getElementById('media-modal')) closeMediaLibrary();
}

function mediaMsg(text, cls) {
  const el = document.getElementById('media-msg');
  if (!el) return;
  el.textContent = text || '';
  el.className = 'pw-msg' + (cls ? ' ' + cls : '');
}

function setMediaFilter(kind, reload = true) {
  mediaLibraryKind = kind || 'all';
  document.querySelectorAll('[data-media-filter]').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.mediaFilter === mediaLibraryKind);
  });
  if (reload) loadMediaLibrary();
}

function _mediaUploadKind() {
  return mediaLibraryKind === 'all' ? 'image' : mediaLibraryKind;
}

async function loadMediaLibrary() {
  const grid = document.getElementById('media-library-grid');
  if (!grid) return;
  grid.innerHTML = '<div class="media-empty">Loading images…</div>';
  try {
    const q = mediaLibraryKind === 'all' ? '' : '?kind=' + encodeURIComponent(mediaLibraryKind);
    mediaLibraryItems = (await apiFetch('/api/media' + q)) || [];
  } catch (e) {
    mediaLibraryItems = [];
    mediaMsg('Could not load images: ' + e.message, 'err');
  }
  renderMediaLibrary();
}

function _mediaCardHTML(item) {
  const selectable = !!mediaLibraryOnSelect;
  const size = item.size ? _formatSizeBytes(item.size, 0) : '';
  return `
    <div class="media-card" data-media-name="${esc(item.name)}">
      <div class="media-thumb"><img src="${esc(item.url)}" alt="" loading="lazy"></div>
      <div class="media-info">
        <span class="media-name" title="${esc(item.name)}">${esc(item.name)}</span>
        <span class="media-meta">${esc(item.kind)}${size ? ' · ' + esc(size) : ''}</span>
      </div>
      <div class="media-actions">
        ${selectable ? '<button type="button" class="btn btn-primary btn-sm" data-media-select>Select</button>' : ''}
        <a class="btn btn-ghost btn-sm" href="${esc(item.url)}?download=1" download="${esc(item.name)}">Download</a>
        ${item.source_url ? '<button type="button" class="btn btn-ghost btn-sm" data-media-refresh>Refresh</button>' : ''}
        <button type="button" class="btn btn-ghost btn-sm" data-media-delete>Delete</button>
      </div>
    </div>`;
}

function renderMediaLibrary() {
  const grid = document.getElementById('media-library-grid');
  if (!grid) return;
  if (!mediaLibraryItems.length) {
    grid.innerHTML = `<div class="media-empty">
      <span>No images in this library yet.</span>
      <span>Upload a file or add one from a URL.</span>
    </div>`;
    return;
  }
  grid.innerHTML = mediaLibraryItems.map(_mediaCardHTML).join('');
}

async function _mediaAfterAdd(item) {
  if (item && mediaLibraryOnSelect && (mediaLibraryKind === 'all' || item.kind === mediaLibraryKind)) {
    try {
      await mediaLibraryOnSelect(item);
      closeMediaLibrary();
      return;
    } catch (e) {
      mediaMsg(e.message, 'err');
    }
  }
  await loadMediaLibrary();
}

function mediaFileChosen(input) {
  const label = document.getElementById('media-file-label');
  if (label) label.dataset.file = input && input.files && input.files[0] ? input.files[0].name : '';
}

async function uploadMedia() {
  const input = document.getElementById('media-file');
  const file = input && input.files && input.files[0];
  if (!file) { mediaMsg('Choose an image first', 'err'); return; }
  const form = new FormData();
  form.append('image', file);
  form.append('kind', _mediaUploadKind());
  mediaMsg('');
  try {
    const item = await apiFetch('/api/media', { method: 'POST', body: form });
    input.value = '';
    const label = document.getElementById('media-file-label');
    if (label) label.dataset.file = '';
    toast('Image uploaded', 'ok');
    await _mediaAfterAdd(item);
  } catch (e) {
    mediaMsg(e.message, 'err');
  }
}

async function addMediaFromURL() {
  const input = document.getElementById('media-url');
  const url = input ? input.value.trim() : '';
  if (!url) { mediaMsg('Enter an image URL', 'err'); return; }
  mediaMsg('');
  try {
    const item = await apiFetch('/api/media/fetch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url, kind: _mediaUploadKind() }),
    });
    input.value = '';
    toast('Image downloaded', 'ok');
    await _mediaAfterAdd(item);
  } catch (e) {
    mediaMsg(e.message, 'err');
  }
}

async function refreshMediaItem(item) {
  try {
    const updated = await apiFetch(`/api/media/${encodeURIComponent(item.name)}/refresh`, { method: 'POST' });
    toast('Image refreshed', 'ok');
    await syncBackgroundConfig();
    await loadMediaLibrary();
    return updated;
  } catch (e) {
    mediaMsg(e.message, 'err');
    return null;
  }
}

async function deleteMediaItem(item) {
  const ok = await confirmDialog({
    title: 'Delete image',
    message: `Delete image "${item.name}"? This removes it from ServerHub.`,
    confirmLabel: 'Delete image',
  });
  if (!ok) return;
  try {
    await apiFetch(`/api/media/${encodeURIComponent(item.name)}`, { method: 'DELETE' });
    toast('Image deleted', 'ok');
    await syncBackgroundConfig();
    await loadMediaLibrary();
  } catch (e) {
    mediaMsg(e.message, 'err');
  }
}

async function removeBackground() {
  try {
    await apiFetch('/api/background', { method: 'DELETE' });
    applyBackgroundConfig('', backgroundBlur);
    bgMsg('Background removed', 'ok');
    toast('Background removed', 'ok');
  } catch (e) {
    bgMsg(e.message, 'err');
  }
}

let _saveTimer = null;
async function _saveAppConfig() {
  clearTimeout(_saveTimer);
  _saveTimer = setTimeout(async () => {
    try {
      await fetch('/api/appconfig', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          hue: accentHue,
          shade: backgroundShade,
          background_color: backgroundColor,
          background_color_enabled: backgroundColorEnabled,
          surface_color: surfaceColor,
          surface_color_enabled: surfaceColorEnabled,
          surface_shade: surfaceShade,
          animations: animationsEnabled,
          size_units: sizeUnits,
          dashboard_order: dashboardOrder,
          dashboard_hidden: dashboardHidden,
          dashboard_days: dashboardShowDays,
        }),
      });
    } catch (_) {}
  }, 400);
}

async function setAnimations(enabled) {
  applyAnimations(enabled);
  _saveAppConfig();
}

// ── Web terminal colors ──────────────────────────────────────────────────────

function _hexColor(v) {
  return typeof v === 'string' && /^#(?:[0-9a-f]{3}|[0-9a-f]{6})$/i.test(v);
}

// _normHex validates and expands #rgb shorthand to #rrggbb (color inputs and
// xterm both want six digits).
function _normHex(v) {
  if (!_hexColor(v)) return null;
  v = v.toLowerCase();
  return v.length === 4
    ? '#' + v[1] + v[1] + v[2] + v[2] + v[3] + v[3]
    : v;
}

function applyTerminalConfig(cfg) {
  if (cfg) {
    for (const key of Object.keys(_termInputSuffix)) {
      const v = _normHex(cfg[key]);
      if (v) terminalConfig[key] = v;
    }
  }
  _syncTerminalInputs();
  _renderTerminalPreview();
}

function _syncTerminalInputs() {
  for (const [key, suffix] of Object.entries(_termInputSuffix)) {
    const colorEl = document.getElementById('term-' + suffix + '-color');
    const hexEl = document.getElementById('term-' + suffix + '-hex');
    if (colorEl) colorEl.value = terminalConfig[key];
    if (hexEl) hexEl.value = terminalConfig[key];
  }
}

function _renderTerminalPreview() {
  const el = document.getElementById('term-preview');
  if (!el) return;
  el.style.background = terminalConfig.background;
  el.style.color = terminalConfig.foreground;
  el.style.setProperty('--term-preview-accent', terminalConfig.accent);
}

function onTerminalColor(key, value) {
  const v = _normHex(value);
  if (!v) return;
  terminalConfig[key] = v;
  _syncTerminalInputs();
  _renderTerminalPreview();
  _saveTerminalConfig();
}

function onTerminalHex(key, value) {
  const v = _normHex(value);
  if (!v) return;
  terminalConfig[key] = v;
  const colorEl = document.getElementById('term-' + _termInputSuffix[key] + '-color');
  if (colorEl) colorEl.value = v;
  _renderTerminalPreview();
  _saveTerminalConfig();
}

function _greyHex(l) {
  const n = Math.round(Math.max(0, Math.min(100, l)) * 255 / 100);
  const h = n.toString(16).padStart(2, '0');
  return '#' + h + h + h;
}

function _rgbHex(rgb) {
  return '#' + rgb.map(v =>
    Math.round(Math.max(0, Math.min(1, v)) * 255).toString(16).padStart(2, '0')
  ).join('');
}

// terminalDefaultsFromTheme maps the current main appearance (accent hue +
// surface shade) onto the terminal colours: page background, primary text
// tone, and the same accent used across the app.
function terminalDefaultsFromTheme() {
  const bg = Math.max(2, Math.min(100, backgroundShade));
  const dark = bg <= SEAM;
  const t = dark ? 1 - bg / SEAM : (bg - SEAM) / (100 - SEAM);
  const surface2 = bg + (dark ? 1 : -1) * (0.4 + 0.6 * t) * 6;
  const textL = dark ? Math.max(91, _tierL(_greyY(surface2), 4.55, true))
                     : Math.min(9,  _tierL(_greyY(surface2), 4.55, false));
  const accentL = accentLightness(accentHue, bg);
  return {
    background: _greyHex(bg),
    foreground: _greyHex(textL),
    accent: _rgbHex(_accentRgb(accentHue, accentL)),
  };
}

function resetTerminalConfig() {
  terminalConfig = terminalDefaultsFromTheme();
  _syncTerminalInputs();
  _renderTerminalPreview();
  _saveTerminalConfig();
  toast('Terminal colors reset to main appearance', 'ok');
}

let _termSaveTimer = null;
function _saveTerminalConfig() {
  clearTimeout(_termSaveTimer);
  _termSaveTimer = setTimeout(async () => {
    try {
      await apiFetch('/api/appconfig', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ terminal: { ...terminalConfig } }),
      });
    } catch (_) {
      toast('Could not save terminal colors', 'err');
    }
  }, 400);
}

async function loadTerminalConfig() {
  try {
    const cfg = await apiFetch('/api/appconfig');
    applyTerminalConfig(cfg.terminal);
  } catch (_) {}
}

// ── State ─────────────────────────────────────────────────────────────────────

let servers = [];
let currentServerId = null;
let pollTimers = {};
const qaRuns = new Map();
let qaDragId = null;
let ownerName = '';
let localHostname = '';
let serverSearch = '';
let localMachine = {
  id: '__local__',
  name: '',
  hostname: '',
  online: true,
  tags: [],
  notes: '',
  favorite: true,
  quick_actions: [],
};
let dashboardES = null;
const latestStats = new Map(); // id → last dashboard snapshot card (survives re-renders)
let detailES = null;
let localDetailES = null;
let jobObs = null; // active { ws, serverId, jobId, text, decoder, follow }
const JOB_SCROLL_CAP = 200000; // characters kept in the observe panel

// ── Animated number counter ───────────────────────────────────────────────────

const _numAnimations = new Map(); // elementId → rafId

function _animateNumber(el, targetStr, durationMs) {
  if (!el) return;
  if (!animationsEnabled) { el.textContent = targetStr; return; }

  // Parse numeric part and suffix (e.g. "42%" → 42, "%")
  const match = targetStr.match(/^([0-9.]+)(.*)$/);
  if (!match) { el.textContent = targetStr; return; }

  const target = parseFloat(match[1]);
  const suffix = match[2];
  const prevMatch = el.textContent.match(/^([0-9.]+)/);
  const start = prevMatch ? parseFloat(prevMatch[1]) : target;

  if (Math.abs(target - start) < 0.5) { el.textContent = targetStr; return; }

  const id = el.dataset.animId || (el.dataset.animId = Math.random().toString(36).slice(2));
  if (_numAnimations.has(id)) cancelAnimationFrame(_numAnimations.get(id));

  const startTime = performance.now();
  const decimals = match[1].includes('.') ? match[1].split('.')[1].length : 0;

  function step(now) {
    const p = Math.min((now - startTime) / durationMs, 1);
    const ease = 1 - Math.pow(1 - p, 3); // cubic ease-out
    const cur = start + (target - start) * ease;
    el.textContent = cur.toFixed(decimals) + suffix;
    if (p < 1) {
      _numAnimations.set(id, requestAnimationFrame(step));
    } else {
      el.textContent = targetStr;
      _numAnimations.delete(id);
    }
  }
  _numAnimations.set(id, requestAnimationFrame(step));
}

// ── Toast ─────────────────────────────────────────────────────────────────────

let toastTimer = null;
function toast(msg, type = 'info') {
  const el = document.getElementById('toast');
  el.textContent = msg;
  el.className = `show toast-${type}`;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { el.className = ''; }, 3000);
}

// ── Custom confirmation ──────────────────────────────────────────────────────

let confirmResolve = null;

function confirmDialog(opts = {}) {
  if (confirmResolve) finishConfirm(false);
  return new Promise(resolve => {
    confirmResolve = resolve;
    document.getElementById('confirm-title').textContent = opts.title || 'Confirm';
    document.getElementById('confirm-message').textContent = opts.message || '';
    const ok = document.getElementById('confirm-ok');
    ok.textContent = opts.confirmLabel || 'Delete';
    ok.disabled = false;
    document.getElementById('confirm-modal').style.display = 'flex';
    document.addEventListener('keydown', confirmKeyHandler);
    ok.focus();
  });
}

function finishConfirm(result) {
  const resolve = confirmResolve;
  confirmResolve = null;
  document.getElementById('confirm-modal').style.display = 'none';
  document.removeEventListener('keydown', confirmKeyHandler);
  if (resolve) resolve(!!result);
}

function confirmOverlayClick(e) {
  if (e.target === document.getElementById('confirm-modal')) finishConfirm(false);
}

function confirmKeyHandler(e) {
  if (e.key === 'Escape') {
    e.preventDefault();
    finishConfirm(false);
  }
}

// ── Navigation ────────────────────────────────────────────────────────────────

let sidebarCollapsed = false;

function applySidebarCollapsed(collapsed) {
  sidebarCollapsed = !!collapsed;
  const app = document.getElementById('app');
  if (app) app.classList.toggle('sidebar-collapsed', sidebarCollapsed);
  const collapseBtn = document.getElementById('sidebar-collapse-btn');
  if (collapseBtn) collapseBtn.setAttribute('aria-expanded', String(!sidebarCollapsed));
  const openBtn = document.getElementById('sidebar-open-btn');
  if (openBtn) openBtn.setAttribute('aria-expanded', String(!sidebarCollapsed));
  try {
    localStorage.setItem('serverhub.sidebarCollapsed', sidebarCollapsed ? '1' : '0');
  } catch (_) {}
}

function toggleSidebar() {
  applySidebarCollapsed(!sidebarCollapsed);
}

function showView(id) {
  if (id !== 'view-schedules') stopDayNowTimer();
  const bg = document.getElementById('main-bg');
  if (bg) {
    bg.style.display = ['view-dashboard', 'view-apps', 'view-servers', 'view-schedules', 'view-docs'].includes(id) ? '' : 'none';
  }
  ['view-dashboard', 'view-servers', 'view-apps', 'view-schedules', 'view-docs', 'view-detail', 'view-settings'].forEach(v => {
    document.getElementById(v).style.display = v === id ? '' : 'none';
  });
  document.getElementById('nav-dashboard').classList.toggle('active', id === 'view-dashboard');
  document.getElementById('nav-servers').classList.toggle('active', id === 'view-servers');
  document.getElementById('nav-apps').classList.toggle('active', id === 'view-apps');
  document.getElementById('nav-schedules').classList.toggle('active', id === 'view-schedules');
  document.getElementById('nav-docs').classList.toggle('active', id === 'view-docs');
  document.getElementById('nav-settings').classList.toggle('active', id === 'view-settings');
  clearAllPolls();
  stopAllQARuns();
}

function showDashboard(push = true) {
  if (push) history.pushState({ view: 'dashboard' }, '', '/');
  showView('view-dashboard');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'ServerHub';
  renderDashboardCards();
  updateDashboardClock();
  loadDashboardWeek();
  startDashboardApps();
  startDashboardStream();
}

function showApps(push = true) {
  if (push) history.pushState({ view: 'apps' }, '', '/apps');
  showView('view-apps');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'App library — ServerHub';
  loadApps();
  startAppsPoll();
}

function showServers(push = true) {
  if (push) history.pushState({ view: 'servers' }, '', '/servers');
  showView('view-servers');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'Servers — ServerHub';
  renderServersCards();
  startDashboardStream();
}

function showSchedules(push = true) {
  if (push) history.pushState({ view: 'schedules' }, '', '/schedules');
  scheduleDay = null;
  scheduleView = 'month';
  showView('view-schedules');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'Schedules — ServerHub';
  renderScheduleViews();
  loadSchedules();
}

function showDocs(push = true) {
  if (push) history.pushState({ view: 'docs' }, '', '/docs');
  showView('view-docs');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'Docs — ServerHub';
}

function showSettings(push = true) {
  if (push) history.pushState({ view: 'settings' }, '', '/settings');
  showView('view-settings');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'Settings — ServerHub';
  showSettingsTab('general');
}

function routeFromLocation() {
  const path = decodeURIComponent(location.pathname.replace(/^\/+|\/+$/g, ''));
  if (!path) {
    showDashboard(false);
    return;
  }
  if (path === 'servers') {
    showServers(false);
    return;
  }
  if (path === 'apps') {
    showApps(false);
    return;
  }
  if (path === 'schedules') {
    showSchedules(false);
    return;
  }
  if (path.startsWith('schedules/')) {
    const date = path.slice('schedules/'.length);
    if (/^\d{4}-\d{2}-\d{2}$/.test(date)) {
      showScheduleDay(date, false);
      return;
    }
  }
  if (path === 'docs') {
    showDocs(false);
    return;
  }
  if (path === 'settings') {
    showSettings(false);
    return;
  }
  if (path === '__local__' || servers.some(s => s.id === path)) {
    showDetail(path, false);
    return;
  }
  history.replaceState({ view: 'dashboard' }, '', '/');
  showDashboard(false);
}

window.addEventListener('popstate', routeFromLocation);

// ── Settings tabs ────────────────────────────────────────────────────────────

function showSettingsTab(name) {
  for (const tab of ['general', 'appearance', 'security', 'terminal']) {
    document.getElementById('settings-' + tab).style.display = name === tab ? '' : 'none';
    document.getElementById('settings-tab-' + tab).classList.toggle('active', name === tab);
  }
  if (name === 'security') {
    refreshSecurity();
    loadAuditLog();
  }
  if (name === 'terminal') {
    loadTerminalConfig();
  }
  if (name === 'appearance') {
    renderDashboardOrderSettings();
  }
}

async function refreshSecurity() {
  const msgEl = document.getElementById('sec-pw-msg');
  msgEl.textContent = '';
  msgEl.className = 'pw-msg';

  try {
    const status = await fetch('/api/auth/status').then(r => r.json());
    updateSignoutVisibility(status);
    document.getElementById('sec-create').style.display = status.has_password ? 'none' : '';
    document.getElementById('sec-change').style.display = status.has_password ? '' : 'none';
  } catch (_) {
    msg('Could not load password status', 'err');
  }
}

function pwMsg(text, cls) {
  const el = document.getElementById('sec-pw-msg');
  el.textContent = text;
  el.className = 'pw-msg' + (cls ? ' ' + cls : '');
}

function pwValid(pw) {
  if (pw.length < 8)  { pwMsg('Password must be at least 8 characters', 'err'); return false; }
  if (pw.length > 72) { pwMsg('Password must be at most 72 bytes', 'err'); return false; }
  return true;
}

async function createPassword() {
  const pw = document.getElementById('sec-create-pw').value;
  if (!pwValid(pw)) return;
  if (pw !== document.getElementById('sec-create-pw2').value) { pwMsg('Passwords do not match', 'err'); return; }
  const btn = document.getElementById('btn-sec-create');
  btn.disabled = true;
  try {
    await apiFetch('/api/auth/setup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: pw }),
    });
    pwMsg('Password set — you now sign in with it.', 'ok');
    toast('Password set', 'ok');
    refreshSecurity();
  } catch (e) {
    pwMsg(e.status === 403 ? 'Setup is only allowed on the machine running ServerHub.' : e.message, 'err');
  } finally {
    btn.disabled = false;
    document.getElementById('sec-create-pw').value = '';
    document.getElementById('sec-create-pw2').value = '';
  }
}

async function changePassword() {
  const oldPw = document.getElementById('sec-change-old').value;
  const newPw = document.getElementById('sec-change-new').value;
  if (!pwValid(newPw)) return;
  if (newPw !== document.getElementById('sec-change-new2').value) { pwMsg('New passwords do not match', 'err'); return; }
  if (newPw === oldPw) { pwMsg('New password must differ from the current one', 'err'); return; }
  const btn = document.getElementById('btn-sec-change');
  btn.disabled = true;
  try {
    await apiFetch('/api/auth/change', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ old_password: oldPw, new_password: newPw }),
    });
    pwMsg('Password changed. Other devices were signed out.', 'ok');
    toast('Password changed', 'ok');
  } catch (e) {
    pwMsg(e.message, 'err');
  } finally {
    btn.disabled = false;
    document.getElementById('sec-change-old').value = '';
    document.getElementById('sec-change-new').value = '';
    document.getElementById('sec-change-new2').value = '';
  }
}

function showDetail(id, push = true) {
  if (push) {
    const from = location.pathname + location.search + location.hash;
    history.pushState({ view: 'detail', id, from }, '', '/' + encodeURIComponent(id));
  }
  currentServerId = id;
  showView('view-detail');
  updateSidebarActive(id);
  renderDetail(id);
  const s = id === '__local__' ? localMachine : servers.find(x => x.id === id);
  document.title = (s ? s.name : id) + ' — ServerHub';
  if (id === '__local__') startLocalDetailStream();
  else startDetailStream(id);
}

function goBackFromDetail() {
  const st = history.state;
  if (st && st.view === 'detail' && st.from) {
    history.back();
    return;
  }
  showServers();
}

// ── Detail tabs ───────────────────────────────────────────────────────────────

let detailTab = 'docker';

function _loadDetailTab() {
  try {
    const saved = localStorage.getItem('serverhub.detailTab');
    if (saved === 'docker' || saved === 'network' || saved === 'system') detailTab = saved;
  } catch (_) {}
}

function showDetailTab(name) {
  if (name !== 'docker' && name !== 'network' && name !== 'system') name = 'docker';
  detailTab = name;
  try { localStorage.setItem('serverhub.detailTab', name); } catch (_) {}
  _renderDetailTab(name);
}

function applyDetailTab() {
  const isLocal = currentServerId === '__local__';
  _renderDetailTab(isLocal ? 'docker' : detailTab);
}

function _renderDetailTab(name) {
  const isLocal = currentServerId === '__local__';
  for (const tab of ['docker', 'network', 'system']) {
    const panel = document.getElementById('detail-panel-' + tab);
    if (panel) panel.style.display = tab === name ? '' : 'none';
    const btn = document.getElementById('detail-tab-' + tab);
    if (btn) {
      btn.classList.toggle('active', tab === name);
      btn.style.display = (isLocal && tab !== 'docker') ? 'none' : '';
    }
  }
}

function updateSidebarActive(id) {
  document.querySelectorAll('.server-nav-item').forEach(el => {
    el.classList.toggle('active', el.dataset.id === id);
  });
}

// ── API helpers ───────────────────────────────────────────────────────────────

async function apiFetch(url, opts = {}) {
  const method = (opts.method || 'GET').toUpperCase();
  if (method !== 'GET') {
    // Mutating requests must carry the preflight-forcing header (CSRF guard).
    opts.headers = Object.assign({ 'X-Requested-With': 'XMLHttpRequest' }, opts.headers);
  }
  const r = await fetch(url, opts);
  if (r.status === 401) {
    // Session expired or missing — go to login, then come back here.
    location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search);
    throw new Error('authentication required');
  }
  if (!r.ok) {
    // Prefer the server's error message when it provides one.
    let message = `HTTP ${r.status}`;
    try {
      const text = await r.text();
      if (text) {
        try {
          const body = JSON.parse(text);
          if (body && body.error) message = body.error;
          else message = text;
        } catch (_) {
          message = text.trim();
        }
      }
    } catch (_) {}
    const err = new Error(message);
    err.status = r.status;
    throw err;
  }
  return r.json();
}

// ── Sidebar ───────────────────────────────────────────────────────────────────

function isFavoriteServer(s) { return !!s && s.favorite === true; }
function isFavoriteLocal() { return localMachine.favorite !== false; }

function _matchSearch(q, values) {
  if (!q) return true;
  return values.some(v => String(v || '').toLowerCase().includes(q));
}

function serverMatches(s, q) {
  return _matchSearch(q, [s.name, s.id, s.host, s.user, (s.tags || []).join(' '), s.notes]);
}

function localMatches(q) {
  return _matchSearch(q, [localMachine.name, localHostname, '__local__', (localMachine.tags || []).join(' '), localMachine.notes]);
}

function _starButton(id, fav) {
  const label = fav ? 'Remove from favorites' : 'Add to favorites';
  return `<button type="button" class="favorite-btn${fav ? ' active' : ''}" data-star="${esc(id)}"
    title="${label}" aria-label="${label}">${icon(fav ? 'star-fill' : 'star', 13)}</button>`;
}

function renderSidebar() {
  const online = servers.filter(s => s.online).length;
  document.getElementById('brand-sub').textContent =
    `${servers.length} server${servers.length !== 1 ? 's' : ''} · ${online} online`;

  const list = document.getElementById('server-list');
  const visible = servers.slice().sort((a, b) => {
    const af = isFavoriteServer(a) ? 0 : 1;
    const bf = isFavoriteServer(b) ? 0 : 1;
    if (af !== bf) return af - bf;
    return String(a.name).localeCompare(String(b.name));
  });
  if (!visible.length) {
    list.innerHTML = `<div class="sidebar-empty">No servers yet</div>`;
    return;
  }
  list.innerHTML = visible.map(s => `
    <div class="server-nav-item${s.id === currentServerId ? ' active' : ''}"
         data-id="${esc(s.id)}">
      <div class="status-dot ${s.online ? 'dot-online' : 'dot-offline'}"></div>
      <span class="server-nav-name">${esc(s.name)}</span>
      ${_starButton(s.id, isFavoriteServer(s))}
    </div>
  `).join('');
}

let _lastWeekKey = '';

// Server clock, fed by the dashboard SSE stream and the /api/local boot fetch.
// Used to show how schedule times on the hub relate to the viewer's clock.
let serverClockInfo = null;

function noteServerClock(clock) {
  if (!clock || typeof clock.time !== 'number') return;
  serverClockInfo = {
    unix: clock.time,
    at: Date.now(),
    offset: clock.utc_offset_minutes | 0,
    tz: clock.tz_name || '',
  };
}

function _serverNow() {
  if (!serverClockInfo) return null;
  return new Date((serverClockInfo.unix + (Date.now() - serverClockInfo.at) / 1000) * 1000);
}

function _fmtUTCOffset(min) {
  const sign = min < 0 ? '−' : '+';
  const abs = Math.abs(min);
  return `UTC${sign}${_pad2(Math.floor(abs / 60))}:${_pad2(abs % 60)}`;
}

function _durLabel(minutes) {
  const abs = Math.abs(minutes);
  const h = Math.floor(abs / 60);
  const m = abs % 60;
  const parts = [];
  if (h) parts.push(`${h}h`);
  if (m) parts.push(`${m}m`);
  return parts.join(' ') || '0m';
}

function renderClockNote() {
  if (!serverClockInfo) return '';
  const meOffset = -new Date().getTimezoneOffset();
  const timeFmt = { hour: '2-digit', minute: '2-digit' };
  const tzPart = serverClockInfo.tz ? ', ' + serverClockInfo.tz : '';
  if (serverClockInfo.offset === meOffset) {
    return `Times shown in your local time — matches the server clock (${_fmtUTCOffset(meOffset)}${tzPart})`;
  }
  const diff = serverClockInfo.offset - meOffset;
  const dir = diff > 0 ? `${_durLabel(diff)} ahead of you` : `${_durLabel(diff)} behind you`;
  const server = _serverNow();
  return `Server ${server ? server.toLocaleTimeString([], timeFmt) : '—'} · `
    + `yours ${new Date().toLocaleTimeString([], timeFmt)} `
    + `(${_fmtUTCOffset(serverClockInfo.offset)}${tzPart} — ${dir})`;
}

function updateTimeNotes() {
  const note = renderClockNote();
  for (const id of ['dashboard-time-note', 'sched-tz-note']) {
    const el = document.getElementById(id);
    if (el) el.textContent = note;
  }
}

function updateDashboardClock() {
  const timeEl = document.getElementById('dashboard-time');
  const dateEl = document.getElementById('dashboard-date');
  if (!timeEl && !dateEl) return;
  const now = new Date();
  if (timeEl) {
    timeEl.textContent = now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit', second: '2-digit' });
  }
  if (dateEl) {
    dateEl.textContent = now.toLocaleDateString([], { weekday: 'long', month: 'long', day: 'numeric', year: 'numeric' });
  }
  const key = now.toDateString();
  if (key !== _lastWeekKey) {
    _lastWeekKey = key;
    renderDashboardWeek();
  }
  updateTimeNotes();
}

function _startOfDay(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

function _dayKey(d) {
  return `${d.getFullYear()}-${_pad2(d.getMonth() + 1)}-${_pad2(d.getDate())}`;
}

function _parseDayKey(key) {
  const parts = String(key || '').split('-').map(Number);
  if (parts.length !== 3 || parts.some(n => Number.isNaN(n))) return new Date();
  return new Date(parts[0], parts[1] - 1, parts[2]);
}

function _sameDay(a, b) {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function _eventChip(e) {
  const t = new Date(e.ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  const reps = e.count > 1 ? ` <span class="week-event-count">×${e.count}</span>` : '';
  const paused = !e.enabled;
  return `<button type="button" class="week-event${paused ? ' paused' : ''}"
          data-sched-detail="${esc(e.schedule.id)}" data-sched-occurrence="${Math.floor(e.ts / 1000)}"
          title="${esc(e.schedule.name)}">
          <span class="week-event-time">${esc(t)}</span>${esc(e.schedule.name)}${reps}</button>`;
}

function _dayEvents(dayStart, dayEnd) {
  let events = _schedEventsForRange(dayStart, dayEnd, dashboardOccurrences);
  if (!_showPausedSchedules()) events = events.filter(e => e.enabled);
  return events;
}

function renderDashboardWeek() {
  const wrap = document.getElementById('dashboard-week');
  if (!wrap) return;

  const now = new Date();
  const start = _startOfDay(now);
  const cells = [];
  for (let i = 0; i < 7; i++) {
    const day = new Date(start.getFullYear(), start.getMonth(), start.getDate() + i);
    const dayStart = day.getTime();
    const dayEnd = new Date(day.getFullYear(), day.getMonth(), day.getDate() + 1).getTime();
    const events = _dayEvents(dayStart, dayEnd);

    const isToday = _sameDay(day, now);
    let eventHTML = '';
    const maxVisible = 4;
    if (events.length > maxVisible) {
      eventHTML = events.slice(0, maxVisible - 1)
        .map(e => _eventChip(e)).join('');
      eventHTML += `<div class="week-event more">+${events.length - (maxVisible - 1)} more</div>`;
    } else {
      eventHTML = events.map(e => _eventChip(e)).join('');
    }

    cells.push(`
      <div class="week-day${isToday ? ' today' : ''}" data-date="${_dayKey(day)}"
           role="button" tabindex="0" title="View schedule for this day">
        <div class="week-day-head">
          <span class="week-day-name${isToday ? ' today' : ''}">${esc(day.toLocaleDateString([], { weekday: 'short' }))}</span>
          <span class="week-day-date">${esc(day.toLocaleDateString([], { month: 'short', day: 'numeric' }))}</span>
        </div>
        <div class="week-day-events">${eventHTML || '<div class="week-event empty">nothing</div>'}</div>
      </div>`);
  }
  wrap.innerHTML = cells.join('');
}

async function loadDashboardWeek() {
  try {
    if (!schedules.length) schedules = (await apiFetch('/api/schedules')) || [];
  } catch (_) {}
  const now = new Date();
  const start = _startOfDay(now);
  const end = new Date(start.getFullYear(), start.getMonth(), start.getDate() + 7);
  // Reads through the shared occurrence cache, so the dashboard week and the
  // Schedules page can never disagree.
  dashboardOccurrences = await getOccurrences(_dayKey(start), _dayKey(end));
  renderDashboardWeek();
}

function localCardHTML(prefix = 'card-') {
  const tags = localMachine.tags && localMachine.tags.length
    ? `<div class="tags">${localMachine.tags.map(t => `<span class="tag">${esc(t)}</span>`).join('')}</div>`
    : '';
  return `
  <div class="server-card local-card" id="${prefix}__local__" data-id="__local__">
    <div class="card-header">
      <span class="card-name">${esc(localHostname)}</span>
      <span class="card-header-actions">
        ${_starButton('__local__', isFavoriteLocal())}
        <span class="badge badge-local" id="${prefix}badge-__local__">local</span>
      </span>
    </div>
    <div class="stat-bars" id="${prefix}bars-__local__">
      <div style="color:var(--text-dim);font-size:11px;">Loading stats…</div>
    </div>
    <div class="card-footer" id="${prefix}footer-__local__">
      <span>—</span><span>—</span>
    </div>
    ${tags}
  </div>`;
}

let apps = [];
let dashboardApps = [];
let appEditId = null;
let appLogoMedia = '';
let appSearch = '';

async function fetchApps() {
  return (await apiFetch('/api/apps')) || [];
}

function _appByID(id) {
  return apps.find(a => a.id === id) || dashboardApps.find(a => a.id === id) || null;
}

function _mediaURL(name) {
  if (!name) return '';
  if (/^https?:\/\//i.test(name) || name.startsWith('/')) return name;
  return '/media/' + encodeURIComponent(name);
}

function _appHost(rawURL) {
  try {
    return new URL(rawURL).host;
  } catch (_) {
    return rawURL;
  }
}

function _appCardHTML(app, compact = false) {
  const initial = String(app.name || '?').trim().charAt(0).toUpperCase();
  const logo = app.logo
    ? `<img class="app-logo" src="${esc(_mediaURL(app.logo))}" alt="" loading="lazy" referrerpolicy="no-referrer">`
    : `<span class="app-logo app-logo-fallback">${esc(initial)}</span>`;
  return `
    <div class="app-card${compact ? ' compact' : ''}" data-app-id="${esc(app.id)}"
         data-app-url="${esc(app.url)}">
      <div class="app-card-top">
        <div class="app-logo-box">${logo}</div>
        <button type="button" class="app-star${app.favorite ? ' active' : ''}"
                data-app-star="${esc(app.id)}"
                title="${app.favorite ? 'Remove from favorites' : 'Add to favorites'}"
                aria-label="${app.favorite ? 'Remove from favorites' : 'Add to favorites'}">
          ${icon(app.favorite ? 'star-fill' : 'star', 14)}
        </button>
      </div>
      <div class="app-card-heading">
        <span class="app-card-name">${esc(app.name)}</span>
        <span class="app-card-server">${esc(_appHost(app.url))}</span>
      </div>
      ${compact ? '' : `<div class="app-actions">
         <a class="btn btn-ghost btn-sm" href="${esc(app.url)}" target="_blank" rel="noopener noreferrer">Open</a>
         <button type="button" class="btn btn-ghost btn-sm" data-app-edit="${esc(app.id)}">Edit</button>
         <button type="button" class="btn btn-ghost btn-sm" data-app-delete="${esc(app.id)}">Delete</button>
       </div>`}
    </div>`;
}

function renderDashboardServices() {
  const wrap = document.getElementById('dashboard-services');
  if (!wrap) return;
  const list = dashboardApps || [];
  if (!list.length) {
    wrap.innerHTML = `
      <button type="button" class="app-more-card empty" onclick="showApps()">
        <span class="app-more-title">No apps yet</span>
        <span class="app-more-sub">Add your first app →</span>
      </button>`;
    return;
  }
  const width = wrap.clientWidth || 1000;
  const perCard = 170;
  const fit = Math.max(1, Math.floor(width / perCard));
  const visible = Math.min(list.length, Math.max(1, fit - 1));
  const shown = list.slice(0, visible);
  const remaining = Math.max(0, list.length - shown.length);
  wrap.innerHTML = shown.map(a => _appCardHTML(a, true)).join('') + `
    <button type="button" class="app-more-card" onclick="showApps()">
      <span class="app-more-title">More</span>
      <span class="app-more-sub">${remaining ? remaining + ' more apps' : 'App library'} →</span>
    </button>`;
}

async function loadDashboardApps() {
  try {
    dashboardApps = await fetchApps();
  } catch (_) {
    dashboardApps = [];
  }
  renderDashboardServices();
}

function startDashboardApps() {
  loadDashboardApps();
}

function renderApps() {
  const grid = document.getElementById('apps-grid');
  const count = document.getElementById('apps-count');
  if (!grid) return;
  const q = appSearch.trim().toLowerCase();
  const list = q
    ? apps.filter(a => [a.name, a.url, _appHost(a.url)].some(v => String(v || '').toLowerCase().includes(q)))
    : apps;
  if (count) count.textContent = apps.length ? `${list.length} of ${apps.length} app${apps.length === 1 ? '' : 's'}` : '';
  if (!apps.length) {
    grid.innerHTML = `<div class="apps-empty">
      <span>No apps yet.</span>
      <span>Add a name and URL and it will appear here.</span>
      <button type="button" class="btn btn-primary btn-sm" onclick="openAppModal()">Add app</button>
    </div>`;
    return;
  }
  if (!list.length) {
    grid.innerHTML = `<div class="apps-empty">
      <span>No apps match “${esc(appSearch)}”.</span>
    </div>`;
    return;
  }
  grid.innerHTML = list.map(a => _appCardHTML(a)).join('');
}

async function loadApps(silent = false) {
  try {
    apps = await fetchApps();
  } catch (e) {
    if (!silent) toast('Could not load apps: ' + e.message, 'err');
    apps = [];
  }
  renderApps();
}

function startAppsPoll() {
  loadApps();
}

async function refreshAppLists() {
  await Promise.all([loadApps(true), loadDashboardApps()]);
}

async function toggleAppFavorite(id, favorite) {
  try {
    await apiFetch(`/api/apps/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ favorite }),
    });
    for (const list of [apps, dashboardApps]) {
      const app = list.find(a => a.id === id);
      if (app) app.favorite = favorite;
    }
    renderApps();
    renderDashboardServices();
  } catch (e) {
    toast('Could not update favorite: ' + e.message, 'err');
  }
}

function openAppWindow(id) {
  const app = _appByID(id);
  if (app) window.open(app.url, '_blank', 'noopener');
}

function openAppModal(id = null) {
  const app = id ? _appByID(id) : null;
  appEditId = app ? app.id : null;
  document.getElementById('app-modal-title').textContent = app ? 'Edit app' : 'New app';
  document.getElementById('app-modal-save').textContent = app ? 'Save changes' : 'Add app';
  document.getElementById('app-modal-delete').style.display = app ? '' : 'none';
  document.getElementById('app-name').value = app ? app.name : '';
  document.getElementById('app-url').value = app ? app.url : '';
  appLogoMedia = '';
  const logoNote = document.getElementById('app-logo-library-note');
  if (logoNote) logoNote.textContent = '';
  const logoSrc = app ? (app.logo_source || (/^https?:\/\//i.test(app.logo || '') ? app.logo : '')) : '';
  document.getElementById('app-logo').value = logoSrc;
  const preview = document.getElementById('app-logo-preview');
  if (preview) {
    preview.src = app && app.logo ? _mediaURL(app.logo) : '';
    preview.style.display = app && app.logo ? '' : 'none';
  }
  const refresh = document.getElementById('app-logo-refresh');
  if (refresh) refresh.style.display = logoSrc ? '' : 'none';
  document.getElementById('app-modal-msg').textContent = '';
  document.getElementById('app-modal').style.display = 'flex';
  document.getElementById('app-name').focus();
  document.addEventListener('keydown', appModalKeyHandler);
}

function closeAppModal() {
  document.getElementById('app-modal').style.display = 'none';
  document.removeEventListener('keydown', appModalKeyHandler);
  appEditId = null;
  appLogoMedia = '';
}

function appModalKeyHandler(e) {
  if (e.key === 'Escape') closeAppModal();
  if (e.key === 'Enter' && e.target && e.target.id === 'app-name') {
    e.preventDefault();
    saveApp();
  }
}

function appOverlayClick(e) {
  if (e.target === document.getElementById('app-modal')) closeAppModal();
}

function appModalMsg(text, cls) {
  const el = document.getElementById('app-modal-msg');
  el.textContent = text || '';
  el.className = 'pw-msg' + (cls ? ' ' + cls : '');
}

function chooseAppLogoFromLibrary() {
  openMediaLibrary({
    kind: 'all',
    title: 'Choose app logo',
    onSelect: item => {
      appLogoMedia = item.name;
      const note = document.getElementById('app-logo-library-note');
      if (note) note.textContent = 'Library image: ' + item.name;
      const preview = document.getElementById('app-logo-preview');
      if (preview) {
        preview.src = item.url + '?v=' + Date.now();
        preview.style.display = '';
      }
      const url = document.getElementById('app-logo');
      if (url) url.value = item.source_url || '';
      const refresh = document.getElementById('app-logo-refresh');
      if (refresh) refresh.style.display = item.source_url ? '' : 'none';
      appModalMsg('');
    },
  });
}

function appLogoURLChanged() {
  if (!appLogoMedia) return;
  appLogoMedia = '';
  const note = document.getElementById('app-logo-library-note');
  if (note) note.textContent = '';
}

async function saveApp() {
  const name = document.getElementById('app-name').value.trim();
  const url = document.getElementById('app-url').value.trim();
  const logo = document.getElementById('app-logo').value.trim();
  if (!name) { appModalMsg('Name is required', 'err'); return; }
  if (!url) { appModalMsg('URL is required', 'err'); return; }
  const btn = document.getElementById('app-modal-save');
  btn.disabled = true;
  try {
    if (appEditId) {
      await apiFetch(`/api/apps/${encodeURIComponent(appEditId)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, url, logo, logo_media: appLogoMedia || '' }),
      });
      toast('App updated', 'ok');
    } else {
      await apiFetch('/api/apps', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name, url, logo, logo_media: appLogoMedia || '' }),
      });
      toast('App added', 'ok');
    }
    closeAppModal();
    await refreshAppLists();
  } catch (e) {
    appModalMsg(e.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

async function refreshAppLogo() {
  if (!appEditId) return;
  try {
    const updated = await apiFetch(`/api/apps/${encodeURIComponent(appEditId)}/logo/refresh`, { method: 'POST' });
    appModalMsg('Logo refreshed', 'ok');
    const preview = document.getElementById('app-logo-preview');
    if (preview) {
      preview.src = _mediaURL(updated.logo) + '?v=' + Date.now();
      preview.style.display = '';
    }
    await refreshAppLists();
  } catch (e) {
    appModalMsg(e.message, 'err');
  }
}

async function deleteApp(id) {
  const app = _appByID(id);
  const ok = await confirmDialog({
    title: 'Delete app',
    message: `Delete app "${app ? app.name : id}"?`,
    confirmLabel: 'Delete app',
  });
  if (!ok) return;
  try {
    await apiFetch(`/api/apps/${encodeURIComponent(id)}`, { method: 'DELETE' });
    closeAppModal();
    await refreshAppLists();
    toast('App deleted', 'ok');
  } catch (e) {
    toast('Could not delete app: ' + e.message, 'err');
  }
}

function deleteAppFromModal() {
  if (appEditId) deleteApp(appEditId);
}

function renderDashboardCards() {
  const favWrap = document.getElementById('favorite-cards');
  if (!favWrap) return;

  const favorites = [];
  if (localHostname && isFavoriteLocal()) favorites.push(localCardHTML());
  for (const s of servers) {
    if (isFavoriteServer(s)) favorites.push(serverCardHTML(s));
  }

  if (favorites.length) {
    favWrap.innerHTML = favorites.join('');
  } else {
    favWrap.innerHTML = '<div class="loading-msg" style="grid-column:1/-1">No favorites yet — star a server to pin it here.</div>';
  }

  renderDashboardQA();
  renderDashboardServices();
  reapplyLatestStats();
}

function dashboardQuickActions() {
  const items = [];
  if (localMachine.quick_actions) {
    localMachine.quick_actions.forEach((qa, idx) => {
      items.push({ serverId: '__local__', serverName: localHostname || 'This Machine', qa, idx });
    });
  }
  for (const s of servers) {
    (s.quick_actions || []).forEach((qa, idx) => {
      items.push({ serverId: s.id, serverName: s.name, qa, idx });
    });
  }
  return items;
}

function renderDashboardQA() {
  const list = document.getElementById('dashboard-qa-list');
  if (!list) return;
  const items = dashboardQuickActions();
  if (!items.length) {
    list.innerHTML = '<div class="qa-empty"><span>No quick actions yet. Add one on a server page.</span></div>';
    return;
  }
  list.innerHTML = items.map(({ serverId, serverName, qa, idx }) => {
    const st = _qaStatus(qa);
    const detached = qa.mode === 'detached';
    return `
      <div class="dash-qa-card" id="dash-qa-item-${esc(serverId)}-${idx}">
        <div class="dash-qa-main">
          <span class="dash-qa-name">${esc(qa.label)}</span>
          <span class="dash-qa-server" data-dash-qa-open="${esc(serverId)}"
                title="Open ${esc(serverName)}">on ${esc(serverName)}</span>
        </div>
        <div class="qa-card-status">
          <span class="qa-action-status ${st.cls}">${st.text}</span>
          <span class="qa-action-spinner"></span>
          <span class="qa-action-duration"></span>
        </div>
        <button type="button" class="qa-play-btn" data-dash-qa-run="${esc(serverId)}"
                data-qa-idx="${idx}">${detached ? 'start' : 'run'}</button>
      </div>`;
  }).join('');
}

function renderServersCards() {
  const wrap = document.getElementById('server-cards');
  if (!wrap) return;

  const q = serverSearch;
  const cards = [];
  if (localHostname && localMatches(q)) cards.push(localCardHTML('srv-card-'));
  for (const s of servers) {
    if (serverMatches(s, q)) cards.push(serverCardHTML(s, 'srv-card-'));
  }

  if (!cards.length) {
    wrap.innerHTML = `<div class="loading-msg" style="grid-column:1/-1">${q ? 'No matching servers.' : 'No servers yet.'}</div>`;
    return;
  }
  wrap.innerHTML = cards.join('');
  reapplyLatestStats();
}

function serverCardHTML(s, prefix = 'card-') {
  const cls = s.online ? '' : ' offline';
  const fav = isFavoriteServer(s);
  return `
  <div class="server-card${cls}" id="${prefix}${esc(s.id)}" data-id="${esc(s.id)}">
    <div class="card-header">
      <span class="card-name">${esc(s.name)}</span>
      <span class="card-header-actions">
        ${_starButton(s.id, fav)}
        <span class="badge ${s.online ? 'badge-online' : 'badge-offline'}" id="${prefix}badge-${esc(s.id)}">
          ${s.online ? 'online' : 'offline'}
        </span>
      </span>
    </div>
    <div class="stat-bars" id="${prefix}bars-${esc(s.id)}">
      ${s.online ? '<div style="color:var(--text-dim);font-size:11px;">Loading stats…</div>' : ''}
    </div>
    <div class="card-footer" id="${prefix}footer-${esc(s.id)}">
      ${s.online ? '<span>—</span><span>—</span>' : '<span>Offline</span><span></span>'}
    </div>
    ${s.tags && s.tags.length ? `<div class="tags">${s.tags.map(t => `<span class="tag">${esc(t)}</span>`).join('')}</div>` : ''}
  </div>`;
}

function applySearch(query) {
  serverSearch = String(query || '').trim().toLowerCase();
  const serversView = document.getElementById('view-servers');
  if (serversView && serversView.style.display !== 'none') renderServersCards();
}

function openFirstSearchResult() {
  const q = serverSearch;
  if (!q) return;
  const server = servers.find(s => serverMatches(s, q));
  if (server) {
    showDetail(server.id);
    return;
  }
  if (localHostname && localMatches(q)) {
    showDetail('__local__');
    return;
  }
  toast('No matching server', 'err');
}

async function toggleFavorite(id) {
  const isLocal = id === '__local__';
  const current = isLocal
    ? isFavoriteLocal()
    : isFavoriteServer(servers.find(s => s.id === id));
  const next = !current;
  try {
    if (isLocal) {
      const result = await apiFetch('/api/local', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ favorite: next }),
      });
      localMachine = {
        ...localMachine,
        ...result,
        favorite: result.favorite,
        tags: result.tags || [],
        quick_actions: result.quick_actions || [],
      };
    } else {
      const result = await apiFetch(`/api/server/${encodeURIComponent(id)}/favorite`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ favorite: next }),
      });
      const idx = servers.findIndex(s => s.id === id);
      if (idx !== -1) servers[idx] = { ...servers[idx], ...result };
    }
    renderSidebar();
    renderDashboardCards();
    renderServersCards();
    toast(next ? 'Added to favorites' : 'Removed from favorites', 'ok');
  } catch (e) {
    toast('Could not update favorite: ' + e.message, 'err');
  }
}

async function loadServers() {
  try {
    servers = await apiFetch('/api/servers');
    if (!servers) servers = [];
    renderSidebar();
    renderDashboardCards();
    renderServersCards();
  } catch (e) {
    renderDashboardCards();
    renderServersCards();
    const box = document.getElementById('server-cards');
    if (box) box.innerHTML = `<div class="loading-msg" style="grid-column:1/-1">Failed to load servers: ${esc(e.message)}</div>`;
  }
}

// ── Dashboard SSE stream ──────────────────────────────────────────────────────

function startDashboardStream() {
  if (dashboardES) return;
  const es = new EventSource('/api/dashboard/events');
  dashboardES = es;
  es.onmessage = (e) => {
    try { applyDashSnapshot(JSON.parse(e.data)); } catch (_) {}
  };
}

function stopDashboardStream() {
  if (dashboardES) { dashboardES.close(); dashboardES = null; }
}

function applyDashSnapshot(snap) {
  if (snap && snap.clock) noteServerClock(snap.clock);
  if (snap.servers) {
    snap.servers.forEach(card => {
      latestStats.set(card.id, card);
      const s = servers.find(x => x.id === card.id);
      if (s) {
        s.online = card.online;
        updateDashboardCard(card.id, card, card.online);
      }
    });
    renderSidebar();
  }
  if (snap.local) {
    latestStats.set('__local__', snap.local);
    let changed = false;
    if (snap.local.name && snap.local.name !== localHostname) {
      localHostname = snap.local.name;
      localMachine.name = snap.local.name;
      changed = true;
    }
    updateDashboardCard('__local__', snap.local, true);
    if (changed) {
      renderSidebar();
      renderDashboardCards();
      renderServersCards();
    }
  }
}

function reapplyLatestStats() {
  for (const [id, stats] of latestStats) {
    updateDashboardCard(id, stats, id === '__local__' ? true : !!stats.online);
  }
}

function updateDashboardCard(id, stats, online) {
  const isLocal = id === '__local__';
  for (const prefix of ['card-', 'srv-card-']) {
    const bars   = document.getElementById(`${prefix}bars-${id}`);
    if (!bars) continue;
    const badge  = document.getElementById(`${prefix}badge-${id}`);
    const footer = document.getElementById(`${prefix}footer-${id}`);
    const card   = document.getElementById(`${prefix}${id}`);

    if (!isLocal && (!online || !stats.online)) {
      card && card.classList.add('offline');
      if (badge)  { badge.textContent = 'offline'; badge.className = 'badge badge-offline'; }
      bars.innerHTML = '';
      if (footer) footer.innerHTML = '<span>Offline</span><span></span>';
      continue;
    }
    card && card.classList.remove('offline');
    if (!isLocal && badge) { badge.textContent = 'online'; badge.className = 'badge badge-online'; }

    const cpu  = stats.cpu  ?? 0;
    const mem  = stats.mem_pct  ?? 0;
    const disk = stats.disk_pct ?? 0;

    // Update bars — reuse existing DOM nodes if present to allow CSS transitions
    const existing = bars.querySelectorAll('.stat-bar-fill');
    if (existing.length === 3) {
      _updateBar(existing[0], cpu,  bars.querySelectorAll('.stat-val')[0], cpu  + '%');
      _updateBar(existing[1], mem,  bars.querySelectorAll('.stat-val')[1], mem  + '%');
      _updateBar(existing[2], disk, bars.querySelectorAll('.stat-val')[2], disk + '%');
    } else {
      bars.innerHTML = barHTML('CPU', cpu, cpu + '%') + barHTML('MEM', mem, mem + '%') + barHTML('DISK', disk, disk + '%');
    }

    if (footer) {
      footer.innerHTML = `<span>up ${formatUptime(stats.uptime)}</span><span>load ${stats.load1 || '—'}</span>`;
    }
  }
}

function _updateBar(fillEl, pct, valEl, valStr) {
  const cls = pct >= 90 ? 'fill-crit' : pct >= 80 ? 'fill-warn' : 'fill-ok';
  fillEl.className = `stat-bar-fill ${cls}`;
  fillEl.style.width = Math.min(100, Math.max(0, pct)) + '%';
  if (valEl) _animateNumber(valEl, valStr, 400);
}

function barHTML(label, pct, valStr) {
  const cls = pct >= 90 ? 'fill-crit' : pct >= 80 ? 'fill-warn' : 'fill-ok';
  const w = Math.min(100, Math.max(0, pct));
  return `
    <div class="stat-row">
      <span class="stat-label">${label}</span>
      <div class="stat-bar-wrap">
        <div class="stat-bar-fill ${cls}" style="width:${w}%"></div>
      </div>
      <span class="stat-val">${valStr}</span>
    </div>`;
}

// ── Detail view ───────────────────────────────────────────────────────────────

function startLocalDetailStream() {
  stopLocalDetailStream();
  fetchAndRenderJobs('__local__');
  clearInterval(pollTimers['__local__jobs']);
  pollTimers['__local__jobs'] = setInterval(() => fetchAndRenderJobs('__local__'), 5000);
  fetchAndRenderContainers('__local__');
  clearInterval(pollTimers['__local__containers']);
  pollTimers['__local__containers'] = setInterval(
    () => fetchAndRenderContainers('__local__'), 5000
  );
  fetchAndRenderImages('__local__');
  clearInterval(pollTimers['__local__images']);
  pollTimers['__local__images'] = setInterval(() => fetchAndRenderImages('__local__'), 300000);
  localDetailES = new EventSource('/api/local/events');
  localDetailES.onmessage = (e) => {
    try {
      const stats = JSON.parse(e.data);
      if (currentServerId !== '__local__') return;
      renderStatGrid('__local__', stats);
    } catch (_) {}
  };
}

function stopLocalDetailStream() {
  if (localDetailES) { localDetailES.close(); localDetailES = null; }
  clearInterval(pollTimers['__local__containers']);
  delete pollTimers['__local__containers'];
  clearInterval(pollTimers['__local__jobs']);
  delete pollTimers['__local__jobs'];
}

function renderDetail(id) {
  const isLocal = id === '__local__';
  const s = isLocal ? localMachine : servers.find(x => x.id === id);
  if (!s) return;
  resetQAOutput();

  const editBtn = document.getElementById('btn-edit-server');
  editBtn.style.display = '';
  editBtn.innerHTML = `${icon('pencil-simple', 12)} Edit`;
  document.getElementById('btn-delete-server').style.display = isLocal ? 'none' : '';

  document.getElementById('detail-name').textContent = s.name;
  document.getElementById('detail-uptime').textContent = '—';

  renderActionRow(s);
  renderQA(s);

  if (s.notes) {
    document.getElementById('notes-section').style.display = '';
    document.getElementById('notes-text').textContent = s.notes;
  } else {
    document.getElementById('notes-section').style.display = 'none';
  }

  document.getElementById('containers-section').style.display = '';
  closeContainerLogs();
  document.getElementById('images-section').style.display = '';
  document.getElementById('images-body').innerHTML =
    '<tr><td colspan="5" style="color:var(--text-dim);font-size:12px;padding:16px 10px;">Loading…</td></tr>';
  document.getElementById('system-section').style.display = isLocal ? 'none' : '';
  document.getElementById('network-section').style.display = isLocal ? 'none' : '';
  if (!isLocal) {
    document.getElementById('probe-output').textContent = 'No probe run yet.';
    document.getElementById('probe-output').classList.remove('probe-error', 'probe-partial');
    renderProbeTargets();
    probeTypeChanged();
  }
  if (!isLocal) {
    document.getElementById('system-body').innerHTML =
      '<div class="loading-msg">Loading system information…</div>';
  }
  document.getElementById('jobs-section').style.display = '';
  closeJobObserver();
  document.getElementById('jobs-list').innerHTML =
    '<div class="jobs-empty">Loading jobs…</div>';
  document.getElementById('stat-grid').innerHTML =
    '<div class="loading-msg">Loading stats…</div>';
  document.getElementById('containers-body').innerHTML =
    '<tr><td colspan="4" style="color:var(--text-dim);font-size:12px;padding:16px 10px;">Loading…</td></tr>';
  applyDetailTab();
}

function renderActionRow(s) {
  document.getElementById('action-row').innerHTML = `
    <a href="/terminal/${encodeURIComponent(s.id)}" target="_blank" class="btn btn-primary">
      ${icon('terminal-window', 14)} Open Terminal
    </a>
    <button class="btn btn-danger" id="btn-reboot-${esc(s.id)}" data-id="${esc(s.id)}" data-act="reboot">
      ${icon('arrow-clockwise', 14)} Reboot
    </button>
    <button class="btn btn-danger" id="btn-shutdown-${esc(s.id)}" data-id="${esc(s.id)}" data-act="shutdown">
      ${icon('power', 14)} Shutdown
    </button>
  `;
}

// _confirmOriginal remembers the replace-with-confirm markup so Cancel can
// restore it without routing it through data strings.
const _confirmOriginal = new Map();

function confirmAction(id, action) {
  const btn = document.getElementById(`btn-${action}-${id}`);
  if (!btn) return;

  const wrap = document.createElement('div');
  wrap.className = 'confirm-wrap';
  wrap.id = `confirm-${action}-${id}`;
  _confirmOriginal.set(`${action}-${id}`, btn.outerHTML);
  wrap.innerHTML = `
    <strong>${action.charAt(0).toUpperCase() + action.slice(1)}?</strong>
    <button class="btn btn-danger btn-sm" data-confirm-yes data-id="${esc(id)}" data-action="${esc(action)}">Yes</button>
    <button class="btn btn-ghost btn-sm" data-confirm-cancel data-id="${esc(id)}" data-action="${esc(action)}">Cancel</button>
  `;
  btn.replaceWith(wrap);
}

function cancelConfirm(id, action) {
  const wrap = document.getElementById(`confirm-${action}-${id}`);
  if (!wrap) return;
  const originalHTML = _confirmOriginal.get(`${action}-${id}`);
  if (originalHTML) wrap.outerHTML = originalHTML;
  _confirmOriginal.delete(`${action}-${id}`);
}

async function runServerAction(id, action) {
  try {
    const url = id === '__local__'
      ? `/api/local/${action}`
      : `/api/server/${encodeURIComponent(id)}/${action}`;
    const res = await apiFetch(url, { method: 'POST' });
    toast(`${action} sent`, res.ok ? 'ok' : 'err');
  } catch (e) {
    toast(`${action} failed: ${e.message}`, 'err');
  }
  renderActionRow(id === '__local__' ? localMachine : servers.find(x => x.id === id));
}

// Stat icons per card label
const _STAT_ICONS = {
  'CPU Usage': 'cpu',
  'Memory':    'hard-drive',
  'Disk (/)':  'hard-drive',
  'Load Average': 'chart-line-up',
};

function renderStatGrid(id, stats) {
  document.getElementById('detail-uptime').textContent = `up ${formatUptime(stats.uptime)}`;

  const memUsed = _formatSizeKB(stats.mem_used);
  const memTotal = _formatSizeKB(stats.mem_total);
  const diskUsed = _formatSizeKB(stats.disk_used);
  const diskTotal = _formatSizeKB(stats.disk_total);

  const cards = [
    { label: 'CPU Usage',    value: `${stats.cpu ?? 0}%`, sub: `load ${stats.load1||'—'} / ${stats.load5||'—'} / ${stats.load15||'—'}`, hist: (stats.history||[]).map(p=>p.cpu) },
    { label: 'Memory',       value: `${stats.mem_pct ?? 0}%`, sub: `${memUsed} / ${memTotal}`, hist: (stats.history||[]).map(p=>p.mem_pct) },
    { label: 'Disk (/)',     value: `${stats.disk_pct ?? 0}%`, sub: `${diskUsed} / ${diskTotal}`, hist: (stats.history||[]).map(p=>p.disk_pct) },
    { label: 'Load Average', value: stats.load1 || '—', sub: `5m: ${stats.load5||'—'} · 15m: ${stats.load15||'—'}`, hist: [] },
  ];

  const grid = document.getElementById('stat-grid');
  const existingCards = grid.querySelectorAll('.stat-card');

  if (existingCards.length === cards.length) {
    // Animate in-place — don't re-render
    cards.forEach((c, i) => {
      const card = existingCards[i];
      const valEl = card.querySelector('.stat-card-value');
      const subEl = card.querySelector('.stat-card-sub');
      const sparkWrap = card.querySelector('.sparkline-wrap');
      if (valEl) _animateNumber(valEl, c.value, 500);
      if (subEl) subEl.textContent = c.sub;
      if (sparkWrap && c.hist.length) sparkWrap.innerHTML = sparklineSVG(c.hist);
    });
  } else {
    grid.innerHTML = cards.map(c => {
      const ico = _STAT_ICONS[c.label];
      return `
      <div class="stat-card">
        <div class="stat-card-label">${ico ? icon(ico, 12) + ' ' : ''}${c.label}</div>
        <div class="stat-card-value">${c.value}</div>
        <div class="stat-card-sub">${c.sub}</div>
        ${c.hist.length ? `<div class="sparkline-wrap">${sparklineSVG(c.hist)}</div>` : ''}
      </div>`;
    }).join('');
  }
}

// ── System info / capabilities ───────────────────────────────────────────────

async function fetchAndRenderSystemInfo(id) {
  if (!id || id === '__local__' || currentServerId !== id) return;
  try {
    const info = await apiFetch(`/api/server/${encodeURIComponent(id)}/info`);
    if (currentServerId === id) renderSystemInfo(info || {});
  } catch (_) {
    const body = document.getElementById('system-body');
    if (body && currentServerId === id) {
      body.innerHTML = '<div class="system-empty">System information unavailable.</div>';
    }
  }
}

function _sizeUnitNames() {
  return sizeUnits === 'GB'
    ? { base: 1000, gb: 'GB', mb: 'MB', kb: 'KB' }
    : { base: 1024, gb: 'GiB', mb: 'MiB', kb: 'KiB' };
}

function _formatSizeBytes(bytes, decimals = 1) {
  const n = Number(bytes) || 0;
  if (!n) return '0 B';
  const u = _sizeUnitNames();
  const gb = n / Math.pow(u.base, 3);
  if (gb >= 1) return gb.toFixed(decimals) + ' ' + u.gb;
  const mb = n / Math.pow(u.base, 2);
  if (mb >= 1) return mb.toFixed(0) + ' ' + u.mb;
  return (n / u.base).toFixed(0) + ' ' + u.kb;
}

function _formatSizeKB(kb) {
  const n = Number(kb) || 0;
  if (!n) return '—';
  return _formatSizeBytes(n * 1024);
}

function _fmtSystemKB(kb) { return _formatSizeKB(kb); }

function _fmtSystemBytes(b) { return _formatSizeBytes(b); }

function applySizeUnits(unit) {
  sizeUnits = unit === 'GB' ? 'GB' : 'GiB';
  const radio = document.querySelector(`input[name="size-units"][value="${sizeUnits}"]`);
  if (radio) radio.checked = true;
}

function setSizeUnits(unit) {
  applySizeUnits(unit);
  renderDashboardCards();
  _saveAppConfig();
}

function _sysItem(label, value) {
  return `<div class="sys-item"><span class="sys-label">${esc(label)}</span><span class="sys-value">${esc(value || '—')}</span></div>`;
}

function _capBadge(label, on) {
  return `<span class="cap-badge ${on ? 'on' : 'off'}">${esc(label)}</span>`;
}

function renderSystemInfo(info) {
  const body = document.getElementById('system-body');
  if (!body) return;
  const s = info.static || {};
  const c = info.capabilities || {};
  if (!s.boot_id && !s.cpu_model && !s.os && !s.mem_total) {
    body.innerHTML = '<div class="system-empty">No system information yet — the host may be offline.</div>';
    return;
  }
  const boot = s.boot_id ? String(s.boot_id).slice(0, 8) : '';
  const updated = s.updated_at ? new Date(s.updated_at * 1000).toLocaleString() : '—';
  const caps = [
    ['tmux', c.tmux], ['docker', c.docker], ['rsync', c.rsync], ['sftp', c.sftp],
    ['iperf3', c.iperf3], ['sudo -n', c.sudo_nopasswd],
  ];
  const disks = (s.disks || []).map(d => `
    <tr>
      <td><span class="container-name">${esc(d.mount)}</span></td>
      <td><span class="container-status">${esc(d.filesystem)}</span></td>
      <td><span class="container-status">${esc(_fmtSystemBytes(d.used))} / ${esc(_fmtSystemBytes(d.total))}</span></td>
      <td><span class="container-status">${esc(String(d.percent ?? 0))}%</span></td>
    </tr>`).join('');
  body.innerHTML = `
    <div class="system-grid">
      ${_sysItem('CPU', s.cpu_model || '—')}
      ${_sysItem('Cores', s.cpu_cores ? String(s.cpu_cores) : '—')}
      ${_sysItem('vCPU', s.cpu_threads ? String(s.cpu_threads) : '—')}
      ${_sysItem('OS', s.os || '—')}
      ${_sysItem('Kernel', s.kernel || '—')}
      ${_sysItem('Architecture', s.arch || '—')}
      ${_sysItem('Memory', _fmtSystemKB(s.mem_total))}
      ${_sysItem('Hostname', s.hostname || '—')}
      ${_sysItem('Boot ID', boot)}
      ${_sysItem('Updated', updated)}
    </div>
    <div class="sys-caps">${caps.map(([label, on]) => _capBadge(label, !!on)).join('')}</div>
    ${disks ? `<div class="table-wrap"><table class="disk-table">
      <thead><tr><th>Mount</th><th>Filesystem</th><th>Used</th><th>Use%</th></tr></thead>
      <tbody>${disks}</tbody></table></div>` : ''}`;
}

// ── Network probes ───────────────────────────────────────────────────────────

function renderProbeTargets() {
  const list = document.getElementById('probe-targets');
  if (!list) return;
  const opts = (servers || []).map(s =>
    `<option value="${esc(s.id)}" label="${esc(s.name)}"></option>`).join('');
  list.innerHTML = opts;
}

function probeTypeChanged() {
  const type = document.getElementById('probe-type').value;
  const hint = document.getElementById('probe-hint');
  const input = document.getElementById('probe-target');
  if (!hint || !input) return;
  switch (type) {
    case 'ping':
      hint.textContent = 'Hostname/IP or a configured server ID. Targeting a server pings both directions.';
      input.placeholder = 'hostname / IP / server ID';
      break;
    case 'dns':
      hint.textContent = 'Resolve a hostname/IP on the server (getent, dig, or nslookup).';
      input.placeholder = 'hostname / IP / server ID';
      break;
    case 'http':
      hint.textContent = 'Time an HTTP request (http:// or https://).';
      input.placeholder = 'https://example.com/health';
      break;
    case 'traceroute':
      hint.textContent = 'Trace the route from the server (traceroute or mtr).';
      input.placeholder = 'hostname / IP / server ID';
      break;
    case 'iperf3':
      hint.textContent = 'Target must be another configured server ID. A 5-second test can transfer roughly 600 MB per direction at 1 Gbps.';
      input.placeholder = 'server ID';
      break;
  }
}

async function runProbe() {
  const id = currentServerId;
  if (!id || id === '__local__') return;
  const type = document.getElementById('probe-type').value;
  const target = document.getElementById('probe-target').value.trim();
  const out = document.getElementById('probe-output');
  const btn = document.getElementById('probe-run');
  if (!target) {
    toast('Enter a probe target', 'err');
    return;
  }
  if (type === 'iperf3') {
    const ok = await confirmDialog({
      title: 'Run iperf3 test',
      message: 'iperf3 transfers real data for 5 seconds. At 1 Gbps that is roughly 600 MB per direction. Run the test now?',
      confirmLabel: 'Run test',
    });
    if (!ok) return;
  }

  btn.disabled = true;
  btn.textContent = 'Running…';
  if (out) {
    out.textContent = 'Running probe…';
    out.classList.remove('probe-error', 'probe-partial');
  }
  try {
    const result = await apiFetch(`/api/server/${encodeURIComponent(id)}/probe`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ type, target }),
    });
    if (currentServerId !== id || !out) return;
    out.textContent = formatProbeResult(result);
    out.classList.toggle('probe-error', result.status === 'error');
    out.classList.toggle('probe-partial', result.status === 'partial');
    toast(`Probe ${result.status}`, result.status === 'error' ? 'err' : 'ok');
  } catch (e) {
    if (out) {
      out.textContent = 'Probe failed: ' + e.message;
      out.classList.add('probe-error');
    }
    toast('Probe failed: ' + e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = 'Run probe';
  }
}

function formatProbeResult(res) {
  const lines = [];
  lines.push(`${String(res.status || 'unknown').toUpperCase()} · ${res.type} · ${res.target} · ${res.duration_ms || 0} ms`);
  if (res.summary) lines.push(res.summary);
  if (res.error) lines.push('Error: ' + res.error);
  if (res.data && res.data.iperf) {
    const d = res.data.iperf;
    const sent = Number(d.sent_mbps || 0).toFixed(1);
    const recv = Number(d.received_mbps || 0).toFixed(1);
    lines.push(`Sent ${sent} Mbps · Received ${recv} Mbps${d.port ? ' · port ' + d.port : ''}`);
  }
  if (res.output) {
    lines.push('');
    lines.push(res.output);
  }
  return lines.join('\n');
}

// ── Docker images ────────────────────────────────────────────────────────────

async function fetchAndRenderImages(id) {
  if (!id || currentServerId !== id) return;
  try {
    const url = id === '__local__'
      ? '/api/local/images'
      : `/api/server/${encodeURIComponent(id)}/images`;
    const list = await apiFetch(url);
    if (currentServerId === id) renderImages(id, list || {});
  } catch (e) {
    const body = document.getElementById('images-body');
    if (body && currentServerId === id) {
      body.innerHTML = `<tr><td colspan="5" class="system-empty">Docker unavailable${e.message ? ': ' + esc(e.message) : ''}</td></tr>`;
    }
  }
}

function renderImages(id, list) {
  const body = document.getElementById('images-body');
  if (!body) return;
  const images = list.images || [];
  if (!images.length) {
    body.innerHTML = '<tr><td colspan="5" class="system-empty">No images found (or Docker unavailable).</td></tr>';
    return;
  }
  body.innerHTML = images.map(img => `
    <tr>
      <td><span class="container-name">${esc(img.repository)}</span></td>
      <td><span class="container-status">${esc(img.tag)}</span></td>
      <td><span class="container-status">${esc(img.id)}</span></td>
      <td><span class="container-status">${esc(img.size)}</span></td>
      <td><span class="container-status">${esc(img.created)}</span></td>
    </tr>`).join('');
}

async function refreshImages() {
  const id = currentServerId;
  if (!id) return;
  try {
    if (id === '__local__') {
      await fetchAndRenderImages(id);
    } else {
      const list = await apiFetch(`/api/server/${encodeURIComponent(id)}/images/refresh`, { method: 'POST' });
      renderImages(id, list || {});
    }
    toast('Images refreshed', 'ok');
  } catch (e) {
    toast('Could not refresh images: ' + e.message, 'err');
  }
}

// ── Container logs ───────────────────────────────────────────────────────────

let containerLog = null;
const CONTAINER_LOG_CAP = 200000;

function openContainerLogs(serverId, name) {
  closeContainerLogs();
  const field = document.getElementById('container-log-field');
  const pre = document.getElementById('container-log-output');
  const title = document.getElementById('container-log-title');
  if (!field || !pre || !title) return;

  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
  const path = serverId === '__local__'
    ? `/api/local/container/${encodeURIComponent(name)}/logs`
    : `/api/server/${encodeURIComponent(serverId)}/container/${encodeURIComponent(name)}/logs`;
  const ws = new WebSocket(`${proto}://${window.location.host}${path}?tail=300`);
  ws.binaryType = 'arraybuffer';

  field.style.display = '';
  title.textContent = `Logs — ${name}`;
  pre.textContent = '';
  pre.classList.remove('empty');
  document.getElementById('container-log-follow').checked = true;
  containerLog = { ws, serverId, name, text: '', decoder: new TextDecoder(), follow: true };

  ws.onmessage = (e) => {
    if (!containerLog || containerLog.ws !== ws) return;
    const chunk = e.data instanceof ArrayBuffer
      ? containerLog.decoder.decode(new Uint8Array(e.data), { stream: true })
      : String(e.data);
    appendContainerLog(chunk);
  };
  ws.onclose = () => {
    if (!containerLog || containerLog.ws !== ws) return;
    try { appendContainerLog(containerLog.decoder.decode()); } catch (_) {}
    containerLog.ws = null;
    title.textContent = `Logs — ${name} (closed)`;
  };
  ws.onerror = () => {
    if (containerLog && containerLog.ws === ws) toast('Log stream error', 'err');
  };
}

function appendContainerLog(chunk) {
  if (!containerLog || !chunk) return;
  containerLog.text = (containerLog.text + chunk).slice(-CONTAINER_LOG_CAP);
  const pre = document.getElementById('container-log-output');
  if (!pre) return;
  pre.textContent = containerLog.text;
  if (containerLog.follow) pre.scrollTop = pre.scrollHeight;
}

function closeContainerLogs() {
  if (containerLog && containerLog.ws) {
    try { containerLog.ws.close(); } catch (_) {}
  }
  containerLog = null;
  const field = document.getElementById('container-log-field');
  if (field) field.style.display = 'none';
}

function clearContainerLogs() {
  if (containerLog) containerLog.text = '';
  const pre = document.getElementById('container-log-output');
  if (pre) pre.textContent = '';
}

function setContainerLogFollow(follow) {
  if (containerLog) containerLog.follow = !!follow;
  const pre = document.getElementById('container-log-output');
  if (pre && follow) pre.scrollTop = pre.scrollHeight;
}

async function fetchAndRenderContainers(id) {
  try {
    const url = id === '__local__'
      ? '/api/local/containers'
      : `/api/server/${encodeURIComponent(id)}/containers`;
    const data = await apiFetch(url);
    renderContainers(id, data.containers || []);
  } catch (_) {
    document.getElementById('containers-body').innerHTML =
      `<tr><td colspan="4" style="color:var(--text-muted);font-size:12px;padding:12px 10px;">Docker unavailable</td></tr>`;
  }
}

function renderContainers(id, containers) {
  const tbody = document.getElementById('containers-body');
  if (!containers.length) {
    tbody.innerHTML = `<tr><td colspan="4" style="color:var(--text-dim);font-size:12px;padding:12px 10px;">No containers found</td></tr>`;
    return;
  }
  tbody.innerHTML = containers.map(c => {
    const running = c.state === 'running';
    return `
    <tr>
      <td><span class="container-name">${esc(c.name)}</span></td>
      <td><span class="badge ${running ? 'badge-running' : 'badge-stopped'}">${esc(c.state)}</span></td>
      <td><span class="container-status">${esc(c.status)}</span></td>
      <td>
        <div style="display:flex;gap:6px">
          <button class="btn btn-ghost btn-sm" data-server="${esc(id)}" data-name="${esc(c.name)}" data-container-action="${running ? 'stop' : 'start'}">${running ? 'Stop' : 'Start'}</button>
          <button class="btn btn-ghost btn-sm" data-server="${esc(id)}" data-name="${esc(c.name)}" data-container-action="restart">Restart</button>
          <button class="btn btn-ghost btn-sm" data-server="${esc(id)}" data-container-logs="${esc(c.name)}">Logs</button>
        </div>
      </td>
    </tr>`;
  }).join('');
}

async function containerAction(serverId, name, action) {
  try {
    const url = serverId === '__local__'
      ? `/api/local/container/${encodeURIComponent(name)}/${action}`
      : `/api/server/${encodeURIComponent(serverId)}/container/${encodeURIComponent(name)}/${action}`;
    const res = await apiFetch(url, { method: 'POST' });
    toast(`${name} ${action}ed`, res.ok ? 'ok' : 'err');
    setTimeout(() => {
      fetchAndRenderContainers(serverId);
      fetchAndRenderImages(serverId);
    }, 1200);
  } catch (e) {
    toast(`Failed: ${e.message}`, 'err');
  }
}

// ── Detached jobs ─────────────────────────────────────────────────────────────

async function fetchAndRenderJobs(serverId) {
  if (!serverId || currentServerId !== serverId) return;
  try {
    const list = await apiFetch(`/api/server/${encodeURIComponent(serverId)}/jobs`);
    if (currentServerId === serverId) renderJobs(serverId, list || []);
  } catch (e) {
    const box = document.getElementById('jobs-list');
    if (box && currentServerId === serverId) {
      box.innerHTML = `<div class="jobs-empty">Could not load jobs: ${esc(e.message)}</div>`;
    }
  }
}

function _jobDuration(job) {
  const end = job.finished_at || Math.floor(Date.now() / 1000);
  return _formatDuration((end - job.started_at) * 1000);
}

function renderJobs(serverId, jobs) {
  const box = document.getElementById('jobs-list');
  if (!box) return;
  if (!jobs.length) {
    box.innerHTML = '<div class="jobs-empty">No jobs yet. Run a detached quick action to start one.</div>';
    return;
  }
  box.innerHTML = jobs.map(job => {
    const running = job.status === 'running';
    const exit = job.exit_code != null ? ` · exit ${job.exit_code}` : '';
    return `
      <div class="job-row">
        <div class="job-main">
          <span class="job-label">${esc(job.label || job.id)}</span>
          <span class="job-meta">${esc(job.id)} · ${esc(job.session)}${esc(exit)}</span>
        </div>
        <span class="job-status job-${esc(job.status)}">${esc(job.status)}</span>
        <span class="job-duration">${esc(_jobDuration(job))}</span>
        <div class="job-actions">
          <button type="button" class="btn btn-ghost btn-sm" data-job-server="${esc(serverId)}" data-job-observe="${esc(job.id)}">observe</button>
          ${serverId === '__local__' ? '' : `<a class="btn btn-ghost btn-sm" href="/terminal/${encodeURIComponent(serverId)}?job=${encodeURIComponent(job.id)}" target="_blank" rel="noopener">attach</a>`}
          ${running ? `<button type="button" class="btn btn-ghost btn-sm" data-job-server="${esc(serverId)}" data-job-stop="${esc(job.id)}">stop</button>` : ''}
          <button type="button" class="btn btn-ghost btn-sm" data-job-server="${esc(serverId)}" data-job-dismiss="${esc(job.id)}">dismiss</button>
        </div>
      </div>`;
  }).join('');
}

function observeJob(serverId, jobId) {
  closeJobObserver();
  const field = document.getElementById('job-output-field');
  const pre = document.getElementById('job-output');
  const title = document.getElementById('job-output-title');
  if (!field || !pre || !title) return;

  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws';
  const url = `${proto}://${window.location.host}/api/server/${encodeURIComponent(serverId)}/jobs/${encodeURIComponent(jobId)}/observe`;
  const ws = new WebSocket(url);
  ws.binaryType = 'arraybuffer';

  field.style.display = '';
  title.textContent = `Output — ${jobId}`;
  pre.textContent = '';
  pre.classList.remove('empty');
  document.getElementById('job-follow').checked = true;
  jobObs = { ws, serverId, jobId, text: '', decoder: new TextDecoder(), follow: true };

  ws.onmessage = (e) => {
    if (!jobObs || jobObs.ws !== ws) return;
    let chunk;
    if (e.data instanceof ArrayBuffer) {
      chunk = jobObs.decoder.decode(new Uint8Array(e.data), { stream: true });
    } else {
      chunk = String(e.data);
    }
    appendJobOutput(chunk);
  };
  ws.onclose = () => {
    if (!jobObs || jobObs.ws !== ws) return;
    try { appendJobOutput(jobObs.decoder.decode()); } catch (_) {}
    jobObs.ws = null;
    title.textContent = `Output — ${jobId} (closed)`;
  };
  ws.onerror = () => {
    if (jobObs && jobObs.ws === ws) toast('Job stream error', 'err');
  };
}

function appendJobOutput(chunk) {
  if (!jobObs || !chunk) return;
  jobObs.text = (jobObs.text + chunk).slice(-JOB_SCROLL_CAP);
  const pre = document.getElementById('job-output');
  if (!pre) return;
  pre.textContent = jobObs.text;
  pre.classList.toggle('empty', !jobObs.text);
  if (jobObs.follow) pre.scrollTop = pre.scrollHeight;
}

function closeJobObserver() {
  if (jobObs && jobObs.ws) {
    try { jobObs.ws.close(); } catch (_) {}
  }
  jobObs = null;
  const field = document.getElementById('job-output-field');
  if (field) field.style.display = 'none';
}

function setJobFollow(follow) {
  if (jobObs) jobObs.follow = !!follow;
  const pre = document.getElementById('job-output');
  if (pre && follow) pre.scrollTop = pre.scrollHeight;
}

function clearJobOutput() {
  if (jobObs) jobObs.text = '';
  const pre = document.getElementById('job-output');
  if (pre) {
    pre.textContent = '';
    pre.classList.add('empty');
  }
}

async function jobAction(serverId, jobId, action) {
  try {
    const url = `/api/server/${encodeURIComponent(serverId)}/jobs/${encodeURIComponent(jobId)}/${action}`;
    await apiFetch(url, { method: 'POST' });
    toast(`Job ${action === 'stop' ? 'stopped' : action}`, 'ok');
    fetchAndRenderJobs(serverId);
  } catch (e) {
    toast(`Could not ${action} job: ${e.message}`, 'err');
  }
}

async function dismissJob(serverId, jobId) {
  try {
    await apiFetch(`/api/server/${encodeURIComponent(serverId)}/jobs/${encodeURIComponent(jobId)}`, {
      method: 'DELETE',
    });
    if (jobObs && jobObs.jobId === jobId && jobObs.serverId === serverId) closeJobObserver();
    toast('Job dismissed', 'ok');
    fetchAndRenderJobs(serverId);
  } catch (e) {
    toast(`Could not dismiss job: ${e.message}`, 'err');
  }
}

function _formatLastRan(ts) {
  if (!ts) return 'Never run';
  const d = new Date(ts * 1000);
  if (Number.isNaN(d.getTime())) return 'Never run';
  return d.toLocaleString();
}

function _qaStatus(qa) {
  if (qa.last_status === 'success') return { cls: 'status-success', text: 'success' };
  if (qa.last_status === 'error') return { cls: 'status-error', text: 'failed' };
  if (qa.last_status === 'stopped') return { cls: 'status-stopped', text: 'stopped' };
  if (qa.last_status === 'started') return { cls: 'status-running', text: 'started' };
  return { cls: 'status-inactive', text: 'inactive' };
}

function _formatDuration(ms) {
  if (!Number.isFinite(ms) || ms < 0) return '';
  if (ms < 1000) return Math.round(ms) + 'ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + 's';
  const m = Math.floor(s / 60);
  return m + 'm ' + String(Math.floor(s % 60)).padStart(2, '0') + 's';
}

function renderQA(s) {
  stopAllQARuns();
  const list = document.getElementById('qa-list');
  const section = document.getElementById('qa-section');
  section.style.display = '';
  if (!s.quick_actions || !s.quick_actions.length) {
    list.innerHTML = `<div class="qa-empty"><span>No quick actions configured. Use the + button to add one.</span></div>`;
    return;
  }
  list.innerHTML = s.quick_actions.map((qa, i) => {
    const st = _qaStatus(qa);
    const detached = qa.mode === 'detached';
    return `
      <div class="qa-card" id="qa-item-${esc(s.id)}-${i}" data-qa-idx="${i}">
        <div class="qa-card-top">
          <div class="qa-card-heading">
            <span class="qa-card-name">${esc(qa.label)}</span>
            ${detached ? '<span class="qa-mode-badge">detached</span>' : ''}
            <span class="qa-card-last">${esc(_formatLastRan(qa.last_ran))}</span>
          </div>
          <span class="qa-drag-handle" draggable="true" data-drag-handle
                title="Drag to reorder" aria-label="Drag to reorder">
            <span></span><span></span>
            <span></span><span></span>
            <span></span><span></span>
          </span>
        </div>
        <div class="qa-card-status">
          <span class="qa-action-status ${st.cls}">${st.text}</span>
          <span class="qa-action-spinner"></span>
          <span class="qa-action-duration"></span>
        </div>
        <div class="qa-card-actions">
          <button type="button" class="qa-play-btn" data-server="${esc(s.id)}" data-qa-idx="${i}" data-qa-run>${detached ? 'start' : 'run'}</button>
          <button type="button" class="qa-edit-btn" data-qa-edit="${i}">edit</button>
        </div>
      </div>`;
  }).join('');
}

function attachQADragHandlers() {
  const list = document.getElementById('qa-list');
  if (!list) return;

  list.addEventListener('dragstart', (e) => {
    const handle = e.target.closest('[data-drag-handle]');
    if (!handle) {
      e.preventDefault();
      return;
    }
    const card = handle.closest('.qa-card');
    if (!card) {
      e.preventDefault();
      return;
    }
    qaDragId = card.id;
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', card.id);
    try { e.dataTransfer.setDragImage(card, 24, 24); } catch (_) {}
    card.classList.add('dragging');
  });

  list.addEventListener('dragover', (e) => {
    if (!qaDragId) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    const dragged = document.getElementById(qaDragId);
    const target = e.target.closest('.qa-card');
    if (!dragged || !target || target === dragged) return;
    const rect = target.getBoundingClientRect();
    const before = e.clientY < rect.top + rect.height / 2;
    if (before) target.before(dragged);
    else target.after(dragged);
  });

  list.addEventListener('drop', (e) => e.preventDefault());

  list.addEventListener('dragend', () => {
    const dragged = qaDragId ? document.getElementById(qaDragId) : null;
    if (dragged) dragged.classList.remove('dragging');
    qaDragId = null;
    commitQAOrder();
  });
}

function commitQAOrder() {
  const machine = currentMachine();
  if (!machine || !machine.quick_actions) return;
  const cards = Array.from(document.querySelectorAll('#qa-list .qa-card'));
  const order = cards.map(c => parseInt(c.dataset.qaIdx, 10));
  if (order.length !== machine.quick_actions.length || order.some(i => Number.isNaN(i))) return;
  if (order.every((idx, i) => idx === i)) return;

  const original = machine.quick_actions.slice();
  machine.quick_actions = order.map(i => original[i]);
  renderQA(machine);
  saveQAOrder(machine, original);
}

async function persistQuickActions(actions) {
  if (currentServerId === '__local__') {
    const result = await apiFetch('/api/local', {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ quick_actions: actions }),
    });
    localMachine = {
      ...localMachine,
      ...result,
      tags: result.tags || [],
      quick_actions: result.quick_actions || [],
    };
  } else {
    const result = await apiFetch(`/api/server/${encodeURIComponent(currentServerId)}/quick-actions`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ quick_actions: actions }),
    });
    const idx = servers.findIndex(x => x.id === currentServerId);
    if (idx !== -1) servers[idx] = { ...result };
  }
  return currentMachine();
}

async function saveQAOrder(machine, original) {
  try {
    await persistQuickActions(machine.quick_actions);
    renderQA(currentMachine());
    toast('Quick action order saved', 'ok');
  } catch (e) {
    machine.quick_actions = original;
    renderQA(machine);
    toast('Could not save order: ' + e.message, 'err');
  }
}

function currentMachine() {
  return currentServerId === '__local__'
    ? localMachine
    : servers.find(x => x.id === currentServerId);
}

let qaModalIndex = null;
let qaDeleteArmed = false;

function qaModalMsg(text, cls) {
  const el = document.getElementById('qa-modal-msg');
  el.textContent = text;
  el.className = 'pw-msg' + (cls ? ' ' + cls : '');
}

function openQAModal(idx = null) {
  const machine = currentMachine();
  if (!machine) return;
  qaModalIndex = idx;
  const editing = idx != null && machine.quick_actions && machine.quick_actions[idx];
  document.getElementById('qa-modal-title').textContent = editing ? 'Edit Quick Action' : 'New Quick Action';
  document.getElementById('qa-modal-label').value = editing ? editing.label : '';
  document.getElementById('qa-modal-command').value = editing ? editing.command : '';
  document.getElementById('qa-modal-mode').value = editing && editing.mode === 'detached' ? 'detached' : 'shell';
  document.getElementById('qa-modal-session').value = editing ? (editing.session || '') : '';
  toggleQAModalMode();
  const delBtn = document.getElementById('qa-modal-delete');
  delBtn.style.display = editing ? '' : 'none';
  delBtn.textContent = 'Delete';
  delBtn.disabled = false;
  qaDeleteArmed = false;
  qaModalMsg('');
  document.getElementById('qa-modal').style.display = 'flex';
  document.getElementById('qa-modal-label').focus();
  document.addEventListener('keydown', qaModalKeyHandler);
}

function toggleQAModalMode() {
  const mode = document.getElementById('qa-modal-mode');
  const group = document.getElementById('qa-modal-session-group');
  if (!mode || !group) return;
  group.style.display = mode.value === 'detached' ? '' : 'none';
}

function closeQAModal() {
  document.getElementById('qa-modal').style.display = 'none';
  document.removeEventListener('keydown', qaModalKeyHandler);
  qaModalIndex = null;
  qaDeleteArmed = false;
  const delBtn = document.getElementById('qa-modal-delete');
  if (delBtn) { delBtn.textContent = 'Delete'; delBtn.disabled = false; }
}

function qaModalOverlayClick(e) {
  if (e.target === document.getElementById('qa-modal')) closeQAModal();
}

function qaModalKeyHandler(e) {
  if (e.key === 'Escape') closeQAModal();
  if (e.key === 'Enter' && e.target && e.target.id === 'qa-modal-command') saveQAModal();
}

async function saveQAModal() {
  const label = document.getElementById('qa-modal-label').value.trim();
  const command = document.getElementById('qa-modal-command').value.trim();
  if (!label || !command) {
    qaModalMsg('Name and command are required', 'err');
    return;
  }
  const machine = currentMachine();
  if (!machine) return;

  const editing = qaModalIndex != null;
  const actions = (machine.quick_actions || []).slice();
  const mode = document.getElementById('qa-modal-mode').value === 'detached' ? 'detached' : '';
  const session = mode === 'detached'
    ? document.getElementById('qa-modal-session').value.trim()
    : '';
  if (editing && qaModalIndex >= 0 && qaModalIndex < actions.length) {
    actions[qaModalIndex] = { ...actions[qaModalIndex], label, command, mode, session };
  } else {
    actions.push({ label, command, mode, session });
  }

  const btn = document.getElementById('qa-modal-save');
  btn.disabled = true;
  btn.textContent = 'Saving…';
  try {
    await persistQuickActions(actions);
    closeQAModal();
    renderQA(currentMachine());
    toast(editing ? 'Quick action updated' : 'Quick action added', 'ok');
  } catch (e) {
    qaModalMsg('Save failed: ' + e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = 'Save';
  }
}

async function deleteQAFromModal() {
  if (qaModalIndex == null) return;
  const btn = document.getElementById('qa-modal-delete');
  if (!qaDeleteArmed) {
    qaDeleteArmed = true;
    btn.textContent = 'Confirm delete';
    return;
  }
  const machine = currentMachine();
  if (!machine) return;
  const actions = (machine.quick_actions || []).slice();
  actions.splice(qaModalIndex, 1);

  btn.disabled = true;
  btn.textContent = 'Deleting…';
  try {
    await persistQuickActions(actions);
    closeQAModal();
    renderQA(currentMachine());
    toast('Quick action deleted', 'ok');
  } catch (e) {
    qaModalMsg('Delete failed: ' + e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = 'Delete';
  }
}

function setQAOutputFor(outputId, text, empty) {
  const out = document.getElementById(outputId);
  if (!out) return;
  out.textContent = text;
  out.classList.toggle('empty', !!empty);
}

function setQAOutput(text, empty) {
  setQAOutputFor('qa-output', text, empty);
}

function resetQAOutput() {
  const field = document.getElementById('qa-output-field');
  if (field) field.classList.remove('running');
  setQAOutput('No output yet.', true);
}

function qaRowId(serverId, idx) {
  return `qa-item-${serverId}-${idx}`;
}

function dashQaRowId(serverId, idx) {
  return `dash-qa-item-${serverId}-${idx}`;
}

function stopQA(rowId) {
  const run = qaRuns.get(rowId);
  if (run) run.controller.abort();
}

function stopAllQARuns() {
  for (const run of qaRuns.values()) {
    clearInterval(run.timer);
    run.controller.abort();
  }
  qaRuns.clear();
  for (const id of ['qa-output-field', 'dash-qa-output-field']) {
    const field = document.getElementById(id);
    if (field) field.classList.remove('running');
  }
}

function toggleQA(serverId, idx) {
  const rowId = qaRowId(serverId, idx);
  if (qaRuns.has(rowId)) stopQA(rowId);
  else runQA(serverId, idx, { rowId, fieldId: 'qa-output-field', outputId: 'qa-output' });
}

function toggleDashboardQA(serverId, idx) {
  const rowId = dashQaRowId(serverId, idx);
  if (qaRuns.has(rowId)) stopQA(rowId);
  else runQA(serverId, idx, { rowId, fieldId: 'dash-qa-output-field', outputId: 'dash-qa-output', dashboard: true });
}

async function runQA(serverId, idx, opts = {}) {
  const rowId = opts.rowId || qaRowId(serverId, idx);
  const fieldId = opts.fieldId || 'qa-output-field';
  const outputId = opts.outputId || 'qa-output';
  const isDashboard = !!opts.dashboard;
  const row = document.getElementById(rowId);
  const field = document.getElementById(fieldId);
  if (!row || !field || qaRuns.has(rowId)) return;

  const statusEl = row.querySelector('.qa-action-status');
  const lastEl = row.querySelector('.qa-card-last');
  const durationEl = row.querySelector('.qa-action-duration');
  const btn = row.querySelector('[data-qa-run], [data-dash-qa-run]');
  const btnLabel = btn ? btn.textContent : '';
  const nameEl = row.querySelector('.qa-card-name, .dash-qa-name');
  const actionLabel = nameEl ? nameEl.textContent : '';
  const controller = new AbortController();
  const started = performance.now();
  const run = { controller, timer: null, fieldId };
  qaRuns.set(rowId, run);

  row.classList.add('running');
  if (statusEl) {
    statusEl.className = 'qa-action-status status-running';
    statusEl.textContent = 'running';
  }
  if (btn) {
    btn.textContent = 'stop';
    btn.classList.add('stop');
  }
  if (durationEl) durationEl.textContent = '0ms';
  run.timer = setInterval(() => {
    if (durationEl) durationEl.textContent = _formatDuration(performance.now() - started);
  }, 200);

  setQAOutputFor(outputId, 'Running…', true);
  field.style.display = '';
  field.classList.add('running');

  let status = 'error';
  try {
    const url = serverId === '__local__'
      ? `/api/local/action/${idx}`
      : `/api/server/${encodeURIComponent(serverId)}/action/${idx}`;
    const r = await fetch(url, {
      method: 'POST',
      headers: { 'X-Requested-With': 'XMLHttpRequest' },
      signal: controller.signal,
    });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const text = await r.text();
    const headerStatus = r.headers.get('X-Quick-Action-Status');
    status = headerStatus === 'error' ? 'error'
      : headerStatus === 'stopped' ? 'stopped'
      : headerStatus === 'started' ? 'started'
      : 'success';
    if (currentServerId === serverId || isDashboard) {
      setQAOutputFor(outputId, text || '(no output)', false);
      if (status === 'started') toast('Job started', 'ok');
      else toast(status === 'success' ? 'Action completed' : 'Action failed',
                 status === 'success' ? 'ok' : 'err');
    }
  } catch (e) {
    if (e.name === 'AbortError') {
      status = 'stopped';
      if (currentServerId === serverId || isDashboard) {
        setQAOutputFor(outputId, 'Stopped.', false);
        toast('Action stopped', 'info');
      }
    } else {
      status = 'error';
      if (currentServerId === serverId || isDashboard) {
        setQAOutputFor(outputId, 'Error: ' + e.message, false);
        toast('Action failed', 'err');
      }
    }
  } finally {
    clearInterval(run.timer);
    qaRuns.delete(rowId);

    const now = Math.floor(Date.now() / 1000);
    const machine = serverId === '__local__'
      ? localMachine
      : servers.find(x => x.id === serverId);
    let qa = machine && machine.quick_actions && machine.quick_actions[idx];
    if (!qa || qa.label !== actionLabel) {
      qa = machine && machine.quick_actions
        ? machine.quick_actions.find(a => a.label === actionLabel)
        : null;
    }
    if (qa) {
      qa.last_ran = now;
      qa.last_status = status;
    }

    if (row.isConnected) {
      row.classList.remove('running');
      if (durationEl) durationEl.textContent = _formatDuration(performance.now() - started);
      if (statusEl) {
        statusEl.className = 'qa-action-status ' + (
          status === 'success' ? 'status-success' :
          status === 'started' ? 'status-running' :
          status === 'stopped' ? 'status-stopped' : 'status-error'
        );
        statusEl.textContent = status === 'success' ? 'success'
          : status === 'started' ? 'started'
          : status === 'stopped' ? 'stopped' : 'failed';
      }
      if (lastEl) lastEl.textContent = _formatLastRan(now);
      if (btn) {
        btn.textContent = btnLabel;
        btn.classList.remove('stop');
      }
    }
    const fieldBusy = Array.from(qaRuns.values()).some(r => r.fieldId === fieldId);
    if ((currentServerId === serverId || isDashboard) && !fieldBusy) {
      field.classList.remove('running');
    }
    if (!isDashboard && status === 'started') fetchAndRenderJobs(serverId);
  }
}

function startDetailStream(id) {
  stopDetailStream();
  fetchAndRenderJobs(id);
  clearInterval(pollTimers[id + 'jobs']);
  pollTimers[id + 'jobs'] = setInterval(() => fetchAndRenderJobs(id), 5000);
  fetchAndRenderSystemInfo(id);
  clearInterval(pollTimers[id + 'info']);
  pollTimers[id + 'info'] = setInterval(() => fetchAndRenderSystemInfo(id), 60000);
  fetchAndRenderImages(id);
  clearInterval(pollTimers[id + 'images']);
  pollTimers[id + 'images'] = setInterval(() => fetchAndRenderImages(id), 300000);
  detailES = new EventSource(`/api/server/${encodeURIComponent(id)}/events`);
  detailES.onmessage = (e) => {
    try {
      const evt = JSON.parse(e.data);
      if (currentServerId !== id) return;
      renderStatGrid(id, evt.stats);
      renderContainers(id, evt.containers || []);
      const s = servers.find(x => x.id === id);
      if (s && evt.stats) s.online = evt.stats.online;
    } catch (_) {}
  };
}

function stopDetailStream() {
  if (detailES) { detailES.close(); detailES = null; }
}

function clearAllPolls() {
  Object.values(pollTimers).forEach(clearInterval);
  pollTimers = {};
  stopDetailStream();
  stopLocalDetailStream();
  closeJobObserver();
  closeContainerLogs();
}

// ── Sparklines (smooth bezier curves) ────────────────────────────────────────

function sparklineSVG(values) {
  if (!values || values.length < 2) return '';
  const W = 300, H = 36, PAD = 2;
  const max = Math.max(...values, 1);

  const pts = values.map((v, i) => ({
    x: PAD + (i / (values.length - 1)) * (W - PAD * 2),
    y: H - PAD - ((v / max) * (H - PAD * 2)),
  }));

  // Smooth cubic bezier path
  let linePath = `M${pts[0].x.toFixed(1)},${pts[0].y.toFixed(1)}`;
  for (let i = 1; i < pts.length; i++) {
    const p0 = pts[i - 1], p1 = pts[i];
    const cx = (p1.x - p0.x) * 0.45;
    linePath += ` C${(p0.x + cx).toFixed(1)},${p0.y.toFixed(1)} ${(p1.x - cx).toFixed(1)},${p1.y.toFixed(1)} ${p1.x.toFixed(1)},${p1.y.toFixed(1)}`;
  }

  // Area path: close to bottom
  const last = pts[pts.length - 1];
  const areaPath = linePath +
    ` L${last.x.toFixed(1)},${H} L${PAD},${H} Z`;

  return `
    <svg class="sparkline" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none">
      <path class="spark-area" d="${areaPath}"/>
      <path class="spark-line" d="${linePath}"/>
    </svg>`;
}

// ── Audit log ─────────────────────────────────────────────────────────────────

async function loadAuditLog() {
  try {
    const entries = await apiFetch('/api/audit');
    const list = document.getElementById('audit-list');
    if (!entries || !entries.length) {
      list.innerHTML = '<div class="audit-empty">No actions taken yet.</div>';
      return;
    }
    list.innerHTML = [...entries].reverse().map(e => `
      <div class="audit-entry">
        <span class="audit-time">${esc(e.time)}</span>
        <span class="audit-server">${esc(e.server_id)}</span>
        <span class="audit-action">${esc(e.action)}</span>
      </div>
    `).join('');
  } catch (_) {}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

function esc(s) {
  return String(s ?? '').replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;');
}

function formatUptime(secs) {
  if (!secs) return '—';
  const d = Math.floor(secs / 86400);
  const h = Math.floor((secs % 86400) / 3600);
  const m = Math.floor((secs % 3600) / 60);
  if (d > 0) return `${d}d ${h}h`;
  if (h > 0) return `${h}h ${m}m`;
  return `${m}m`;
}

// ── Modal — Add / Edit server ─────────────────────────────────────────────────

let modalMode = 'add';
let modalEditId = null;
let qaRowCount = 0;

function setModalLocalMode(isLocal) {
  for (const id of ['fg-host', 'fg-port', 'server-ssh-row']) {
    const el = document.getElementById(id);
    if (el) el.style.display = isLocal ? 'none' : '';
  }
}

function openAddServer() {
  modalMode = 'add';
  modalEditId = null;
  document.getElementById('modal-title').textContent = 'Add Server';
  document.getElementById('modal-save-btn').textContent = 'Add Server';
  setModalLocalMode(false);
  clearForm();
  document.getElementById('server-modal').style.display = 'flex';
  document.getElementById('f-name').focus();
  document.addEventListener('keydown', modalKeyHandler);
}

function openEditServer() {
  const s = servers.find(x => x.id === currentServerId);
  if (!s) return;
  modalMode = 'edit';
  modalEditId = s.id;
  document.getElementById('modal-title').textContent = 'Edit Server';
  document.getElementById('modal-save-btn').textContent = 'Save Changes';
  setModalLocalMode(false);
  populateForm(s);
  document.getElementById('server-modal').style.display = 'flex';
  document.getElementById('f-name').focus();
  document.addEventListener('keydown', modalKeyHandler);
}

function openLocalEdit() {
  modalMode = 'local';
  modalEditId = '__local__';
  document.getElementById('modal-title').textContent = 'Edit This Machine';
  document.getElementById('modal-save-btn').textContent = 'Save Changes';
  setModalLocalMode(true);
  populateForm(localMachine);
  document.getElementById('server-modal').style.display = 'flex';
  document.getElementById('f-name').focus();
  document.addEventListener('keydown', modalKeyHandler);
}

function closeModal() {
  document.getElementById('server-modal').style.display = 'none';
  document.removeEventListener('keydown', modalKeyHandler);
}

function modalOverlayClick(e) {
  if (e.target === document.getElementById('server-modal')) closeModal();
}

function modalKeyHandler(e) { if (e.key === 'Escape') closeModal(); }

function clearForm() {
  document.getElementById('f-name').value = '';
  document.getElementById('f-host').value = '';
  document.getElementById('f-port').value = '22';
  document.getElementById('f-user').value = '';
  document.getElementById('f-key').value = '~/.ssh/id_ed25519';
  document.getElementById('f-tags').value = '';
  document.getElementById('f-notes').value = '';
  document.getElementById('qa-form-list').innerHTML = '';
  qaRowCount = 0;
}

function populateForm(s) {
  document.getElementById('f-name').value  = s.name  || '';
  document.getElementById('f-host').value  = s.host  || '';
  document.getElementById('f-port').value  = s.port  || 22;
  document.getElementById('f-user').value  = s.user  || '';
  document.getElementById('f-key').value   = s.key   || '~/.ssh/id_ed25519';
  document.getElementById('f-tags').value  = (s.tags || []).join(', ');
  document.getElementById('f-notes').value = s.notes || '';
  document.getElementById('qa-form-list').innerHTML = '';
  qaRowCount = 0;
  (s.quick_actions || []).forEach(qa => addQARow(qa.label, qa.command, qa.mode, qa.session, qa.id));
}

function addQARow(label = '', command = '', mode = '', session = '', id = '') {
  const rowId = qaRowCount++;
  const row = document.createElement('div');
  row.className = 'qa-form-row';
  row.id = `qa-row-${rowId}`;
  row.dataset.qaId = id || '';
  row.innerHTML = `
    <input class="form-input qa-form-label" type="text" placeholder="Label" value="${esc(label)}" data-qa-field="label">
    <input class="form-input qa-form-cmd" type="text" placeholder="Command" value="${esc(command)}" data-qa-field="command" style="font-family:var(--font-mono);font-size:12px">
    <select class="form-input qa-form-mode" data-qa-field="mode" title="Mode">
      <option value="shell">shell</option>
      <option value="detached">detached</option>
    </select>
    <input class="form-input qa-form-session" type="text" placeholder="session" value="${esc(session)}" data-qa-field="session" style="font-family:var(--font-mono);font-size:12px">
    <button type="button" class="qa-remove-btn" onclick="removeQARow('qa-row-${rowId}')" title="Remove">
      ${icon('x', 12)}
    </button>`;
  document.getElementById('qa-form-list').appendChild(row);
  const modeSelect = row.querySelector('[data-qa-field="mode"]');
  modeSelect.value = mode === 'detached' ? 'detached' : 'shell';
  if (modalMode === 'local') {
    const detached = modeSelect.querySelector('option[value="detached"]');
    if (detached) detached.disabled = true;
    modeSelect.value = 'shell';
  }
  qaRowModeChanged(modeSelect);
}

function qaRowModeChanged(select) {
  const row = select.closest('.qa-form-row');
  if (!row) return;
  const session = row.querySelector('[data-qa-field="session"]');
  const detached = select.value === 'detached';
  if (session) {
    session.style.display = detached ? '' : 'none';
    if (!detached) session.value = '';
  }
}

function removeQARow(rowId) {
  const el = document.getElementById(rowId);
  if (el) el.remove();
}

function collectForm() {
  const name    = document.getElementById('f-name').value.trim();
  const host    = document.getElementById('f-host').value.trim();
  const port    = parseInt(document.getElementById('f-port').value) || 22;
  const user    = document.getElementById('f-user').value.trim();
  const key     = document.getElementById('f-key').value.trim();
  const tagsRaw = document.getElementById('f-tags').value.trim();
  const notes   = document.getElementById('f-notes').value.trim();
  const tags = tagsRaw ? tagsRaw.split(',').map(t => t.trim()).filter(Boolean) : [];
  const qaRows = document.querySelectorAll('#qa-form-list .qa-form-row');
  const quick_actions = Array.from(qaRows).map(row => {
    const mode = row.querySelector('[data-qa-field="mode"]').value === 'detached' ? 'detached' : '';
    return {
      id: row.dataset.qaId || undefined,
      label:   row.querySelector('[data-qa-field="label"]').value.trim(),
      command: row.querySelector('[data-qa-field="command"]').value.trim(),
      mode,
      session: mode === 'detached'
        ? row.querySelector('[data-qa-field="session"]').value.trim()
        : '',
    };
  }).filter(qa => qa.label && qa.command);
  return { name, host, port, user, key, tags, notes, quick_actions };
}

async function saveServer() {
  const data = collectForm();
  if (!data.name) { toast('Name is required', 'err'); return; }
  if (modalMode !== 'local') {
    if (!data.host) { toast('Host is required', 'err'); return; }
    if (!data.user) { toast('SSH user is required', 'err'); return; }
  }

  const btn = document.getElementById('modal-save-btn');
  btn.disabled = true;
  btn.textContent = 'Saving…';

  try {
    let result;
    if (modalMode === 'add') {
      result = await apiFetch('/api/servers', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });
      servers.push({ ...result });
      toast(`${result.name} added`, 'ok');
    } else if (modalMode === 'local') {
      result = await apiFetch('/api/local', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: data.name,
          tags: data.tags,
          notes: data.notes,
          quick_actions: data.quick_actions,
        }),
      });
      localMachine = {
        ...localMachine,
        ...result,
        tags: result.tags || [],
        quick_actions: result.quick_actions || [],
      };
      localHostname = result.name || localHostname;
      toast('This Machine updated', 'ok');
    } else {
      result = await apiFetch(`/api/server/${modalEditId}`, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(data) });
      const idx = servers.findIndex(x => x.id === modalEditId);
      if (idx !== -1) servers[idx] = { ...result };
      toast(`${result.name} updated`, 'ok');
    }
    const mode = modalMode;
    closeModal();
    renderSidebar();
    renderDashboardCards();
    renderServersCards();
    if (mode === 'edit' && currentServerId === modalEditId) renderDetail(modalEditId);
    else if (mode === 'local' && currentServerId === '__local__') renderDetail('__local__');
  } catch (e) {
    toast('Save failed: ' + e.message, 'err');
  } finally {
    btn.disabled = false;
    btn.textContent = modalMode === 'add' ? 'Add Server' : 'Save Changes';
  }
}

// ── Delete server ─────────────────────────────────────────────────────────────

function confirmDeleteServer() {
  const s = servers.find(x => x.id === currentServerId);
  if (!s) return;
  document.getElementById('delete-server-name').textContent = s.name;
  document.getElementById('delete-confirm-input').value = '';
  document.getElementById('delete-server-confirm').disabled = true;
  document.getElementById('delete-modal').style.display = 'flex';
  document.getElementById('delete-confirm-input').focus();
  document.addEventListener('keydown', deleteKeyHandler);
}

function updateDeleteConfirm() {
  const value = document.getElementById('delete-confirm-input').value.trim().toLowerCase();
  document.getElementById('delete-server-confirm').disabled = value !== 'delete';
}

function deleteConfirmKeydown(e) {
  if (e.key === 'Enter' && !document.getElementById('delete-server-confirm').disabled) {
    e.preventDefault();
    deleteServer();
  }
}

function closeDeleteModal() {
  document.getElementById('delete-modal').style.display = 'none';
  document.removeEventListener('keydown', deleteKeyHandler);
}

function deleteOverlayClick(e) {
  if (e.target === document.getElementById('delete-modal')) closeDeleteModal();
}

function deleteKeyHandler(e) { if (e.key === 'Escape') closeDeleteModal(); }

async function deleteServer() {
  const id = currentServerId;
  if (!id) return;
  const typed = document.getElementById('delete-confirm-input').value.trim().toLowerCase();
  if (typed !== 'delete') return;
  try {
    await apiFetch(`/api/server/${id}`, { method: 'DELETE' });
    servers = servers.filter(x => x.id !== id);
    closeDeleteModal();
    renderSidebar();
    renderServersCards();
    showDashboard();
    toast('Server removed', 'ok');
  } catch (e) {
    toast('Delete failed: ' + e.message, 'err');
  }
}

// ── Local machine edit ────────────────────────────────────────────────────────

function handleEditBtn() {
  if (currentServerId === '__local__') openLocalEdit();
  else openEditServer();
}

// ── Sign out ──────────────────────────────────────────────────────────────────

// updateSignoutVisibility shows the sidebar Sign out button only when the
// instance is password-protected (opt-out instances have no sessions to end).
function updateSignoutVisibility(status) {
  const signout = document.getElementById('nav-signout');
  if (signout) signout.style.display = status.has_password ? '' : 'none';
}

async function signOut() {
  try {
    await fetch('/api/auth/logout', {
      method: 'POST',
      headers: { 'X-Requested-With': 'XMLHttpRequest' },
    });
  } catch (_) {}
  location.href = '/login';
}

// ── Schedules ────────────────────────────────────────────────────────────────

let schedules = [];
let scheduleEditId = null;
let scheduleMonth = new Date(new Date().getFullYear(), new Date().getMonth(), 1);
let scheduleDay = null; // 'YYYY-MM-DD' when the hourly day view is open
let scheduleView = 'month'; // 'month' | 'list'
let scheduleOccurrences = [];
let dashboardOccurrences = [];
let scheduleDetailId = null;
let scheduleDetailOccurrence = null;
let schedulePreviewTimer = null;
let schedulePreviewSeq = 0;

const SCHED_DAYS = ['mon', 'tue', 'wed', 'thu', 'fri', 'sat', 'sun'];
const DAY_HOUR_PX = 60;
let dayNowTimer = null;
let dayHoverMinute = null;
let dayNowMinute = null;

function _pad2(n) { return String(n).padStart(2, '0'); }
function _schedTodayKey() { return _dayKey(new Date()); }
function _schedTimeValue(t) { return /^\d{2}:\d{2}$/.test(String(t || '')) ? t : '03:00'; }
function _schedDateValue(d) { return /^\d{4}-\d{2}-\d{2}$/.test(String(d || '')) ? d : _schedTodayKey(); }

function _parseDurationMs(raw) {
  const text = String(raw || '').trim();
  if (!text) return 0;
  const m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(text);
  if (!m || (!m[1] && !m[2] && !m[3])) return 0;
  return (parseInt(m[1] || '0', 10) * 3600 +
          parseInt(m[2] || '0', 10) * 60 +
          parseFloat(m[3] || '0')) * 1000;
}

function _scheduleById(id) { return (schedules || []).find(s => s.id === id) || null; }
function _schedNextRun(s) { return (s && s.next_runs && s.next_runs[0]) || (s && s.next_run) || 0; }
function _schedEnabled(s) { return !s || s.enabled !== false; }

function _schedOccurrencesBetween(startMs, endMs, list = scheduleOccurrences) {
  const out = [];
  for (const o of list || []) {
    const ts = Number(o.time) * 1000;
    if (ts >= startMs && ts < endMs) out.push(o);
  }
  out.sort((a, b) => a.time - b.time || String(a.schedule_id).localeCompare(String(b.schedule_id)));
  return out;
}

function _schedEventsForRange(startMs, endMs, list = scheduleOccurrences) {
  const map = new Map();
  for (const o of _schedOccurrencesBetween(startMs, endMs, list)) {
    const s = _scheduleById(o.schedule_id);
    if (!s) continue;
    const cur = map.get(s.id);
    if (cur) {
      cur.count++;
    } else {
      map.set(s.id, {
        ts: Number(o.time) * 1000,
        count: 1,
        schedule: s,
        enabled: o.enabled !== false && _schedEnabled(s),
      });
    }
  }
  return [...map.values()].sort((a, b) => a.ts - b.ts);
}

function _schedDaysLabel(days) {
  const labels = { mon: 'Mon', tue: 'Tue', wed: 'Wed', thu: 'Thu', fri: 'Fri', sat: 'Sat', sun: 'Sun' };
  return (days || []).map(d => labels[d] || d).join(', ');
}

function _scheduleCadence(s) {
  if (!s) return '—';
  if (s.date) return `once ${s.date} ${s.at || ''}`.trim();
  if (s.days && s.days.length) {
    const weekdays = ['mon', 'tue', 'wed', 'thu', 'fri'];
    const isWeekdays = s.days.length === weekdays.length && weekdays.every(d => s.days.includes(d));
    return isWeekdays ? `weekdays @ ${s.at || '—'}` : `${_schedDaysLabel(s.days)} @ ${s.at || '—'}`;
  }
  if (s.cron) return 'cron: ' + s.cron;
  const ms = _parseDurationMs(s.every);
  if (s.at && ms >= 86400000 && ms % 86400000 === 0) {
    const days = ms / 86400000;
    return days === 1 ? `daily @ ${s.at}` : `every ${days} days @ ${s.at}`;
  }
  if (ms >= 3600000 && ms % 3600000 === 0) return `every ${ms / 3600000}h`;
  if (ms >= 60000 && ms % 60000 === 0) return `every ${ms / 60000}m`;
  if (ms > 0) return `every ${s.every}`;
  if (s.at) return `daily @ ${s.at}`;
  return '—';
}

function _scheduleActionLabel(s) {
  if (!s) return '—';
  if (s.quick_action) return 'action: ' + s.quick_action;
  if (s.command) return s.command;
  return '—';
}

function _scheduleTargetsLabel(s) {
  const parts = [];
  if (s && s.targets && s.targets.length) parts.push(s.targets.map(id => _serverLabel(id)).join(', '));
  if (s && s.tags && s.tags.length) parts.push('tags: ' + s.tags.join(', '));
  return parts.join(' · ') || '—';
}

function _serverLabel(id) {
  if (id === '__local__') return (localMachine && localMachine.name) || localHostname || 'This machine';
  const s = (servers || []).find(x => x.id === id);
  return s ? (s.name || s.id) : id;
}

function _fmtScheduleWhen(ts) {
  if (!ts) return '—';
  const d = new Date(Number(ts) * 1000);
  if (Number.isNaN(d.getTime())) return '—';
  return d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

function _schedStatusClass(s) {
  if (s && s.running) return 'running';
  const st = (s && s.last_status) || '';
  return ['success', 'partial', 'error', 'skipped', 'missed'].includes(st) ? st : 'none';
}

function _schedDotClass(s) {
  if (s && s.running) return 'running';
  return _schedStatusClass(s);
}

function _schedStatusBadge(s) {
  const st = s && s.running ? 'running' : ((s && s.last_status) || 'none');
  return `<span class="sched-badge ${esc(st)}">${esc(st)}</span>`;
}

function _schedChip(e, extraClass = '') {
  const s = e.schedule;
  const paused = !e.enabled;
  const time = new Date(e.ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  return `<button type="button" class="cal-event${paused ? ' paused' : ''}${extraClass ? ' ' + extraClass : ''}"
          data-sched-detail="${esc(s.id)}" data-sched-occurrence="${Math.floor(e.ts / 1000)}"
          title="${esc(s.name)} — ${esc(_scheduleCadence(s))}">
    <span class="cal-ev-dot ${esc(_schedDotClass(s))}"></span><span class="cal-ev-time">${esc(time)}</span>${esc(s.name)}${e.count > 1 ? ` ×${e.count}` : ''}
  </button>`;
}

function _showPausedSchedules() {
  const el = document.getElementById('sched-show-paused');
  return !el || el.checked;
}

function setScheduleView(view) {
  scheduleView = view === 'list' ? 'list' : 'month';
  scheduleDay = null;
  if (scheduleView === 'month') {
    loadScheduleOccurrences().then(renderScheduleViews);
  } else {
    renderScheduleViews();
  }
}

function renderScheduleViews() {
  const month = document.getElementById('schedule-month-view');
  const day = document.getElementById('schedule-day-view');
  const list = document.getElementById('schedule-list-view');
  const monthBtn = document.getElementById('sched-view-month');
  const listBtn = document.getElementById('sched-view-list');
  if (monthBtn) monthBtn.classList.toggle('active', scheduleView === 'month');
  if (listBtn) listBtn.classList.toggle('active', scheduleView === 'list');

  const showDay = scheduleView === 'month' && !!scheduleDay;
  const toolbar = document.querySelector('.schedules-toolbar');
  const viewToggle = document.getElementById('sched-view-toggle');
  const showPaused = document.getElementById('sched-show-paused');
  const showPausedWrap = showPaused ? showPaused.closest('.sched-show-paused') : null;
  const newBtn = document.getElementById('schedules-new-btn');
  if (toolbar) toolbar.style.display = showDay ? 'none' : '';
  if (viewToggle) viewToggle.style.display = showDay ? 'none' : '';
  if (showPausedWrap) showPausedWrap.style.display = (!showDay && scheduleView === 'month') ? '' : 'none';
  if (newBtn) newBtn.style.display = showDay ? 'none' : '';

  if (month) month.style.display = scheduleView === 'month' && !showDay ? '' : 'none';
  if (day) day.style.display = showDay ? '' : 'none';
  if (list) list.style.display = scheduleView === 'list' ? '' : 'none';

  if (scheduleView === 'list') {
    stopDayNowTimer();
    renderScheduleList();
  } else if (showDay) {
    renderScheduleDay();
  } else {
    stopDayNowTimer();
    renderScheduleCalendar();
  }
  renderDashboardWeek();
}

function renderScheduleCalendar() {
  const grid = document.getElementById('schedule-calendar');
  if (!grid) return;
  const label = document.getElementById('calendar-label');
  if (label) {
    label.textContent = scheduleMonth.toLocaleDateString([], { month: 'long', year: 'numeric' });
  }

  const year = scheduleMonth.getFullYear();
  const month = scheduleMonth.getMonth();
  const first = new Date(year, month, 1);
  const startOffset = (first.getDay() + 6) % 7; // Monday-first week
  const daysInMonth = new Date(year, month + 1, 0).getDate();
  const today = new Date();
  const todayStart = new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime();

  const head = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
    .map(d => `<div class="cal-head">${d}</div>`).join('');

  const cells = [];
  for (let i = 0; i < startOffset; i++) cells.push('<div class="cal-cell cal-blank" aria-hidden="true"></div>');

  for (let dayNum = 1; dayNum <= daysInMonth; dayNum++) {
    const day = new Date(year, month, dayNum);
    const dayStart = day.getTime();
    const dayEnd = new Date(year, month, dayNum + 1).getTime();
    let events = _schedEventsForRange(dayStart, dayEnd);
    if (!_showPausedSchedules()) events = events.filter(e => e.enabled);

    const isToday = _sameDay(day, today);
    const isPast = day.getTime() < todayStart;
    const visible = 4;
    let eventsHTML = '';
    if (events.length > visible) {
      eventsHTML = events.slice(0, visible - 1).map(e => _schedChip(e)).join('');
      eventsHTML += `<button type="button" class="cal-more"
        title="View all ${events.length} runs for ${_dayKey(day)} in the hourly view"
        aria-label="Show ${events.length - (visible - 1)} more schedules for this day">+${events.length - (visible - 1)} more</button>`;
    } else {
      eventsHTML = events.map(e => _schedChip(e)).join('');
    }

    cells.push(`
      <div class="cal-cell${isToday ? ' today' : ''}${isPast ? ' past' : ''}" data-day="${dayNum}" data-date="${_dayKey(day)}">
        <div class="cal-day${isToday ? ' today' : ''}"><span>${dayNum}</span></div>
        ${eventsHTML}
      </div>`);
  }

  for (let i = cells.length; i % 7 !== 0; i++) {
    cells.push('<div class="cal-cell cal-blank" aria-hidden="true"></div>');
  }

  grid.innerHTML = head + cells.join('');
}

function stopDayNowTimer() {
  if (dayNowTimer) {
    clearInterval(dayNowTimer);
    dayNowTimer = null;
  }
}

function renderDayNowLine() {
  const line = document.getElementById('day-now-line');
  const label = document.getElementById('day-now-label');
  if (!line) return;
  const now = new Date();
  if (!scheduleDay || !_sameDay(_parseDayKey(scheduleDay), now)) {
    dayNowMinute = null;
    line.style.display = 'none';
    _refreshDayLabelFade();
    return;
  }
  const minute = now.getHours() * 60 + now.getMinutes() + now.getSeconds() / 60;
  dayNowMinute = minute;
  line.style.top = minute + 'px';
  line.style.display = '';
  if (label) label.textContent = `${_pad2(now.getHours())}:${_pad2(now.getMinutes())}`;
  _refreshDayLabelFade();
}

function _refreshDayLabelFade() {
  const sheet = document.getElementById('day-sheet');
  if (!sheet) return;
  const minutes = [];
  if (dayHoverMinute != null) minutes.push(dayHoverMinute);
  if (dayNowMinute != null) minutes.push(dayNowMinute);
  sheet.querySelectorAll('.day-hour-line').forEach(el => {
    const hourMinute = Number(el.dataset.minute);
    const fade = minutes.some(m => Math.abs(hourMinute - m) < 12);
    el.classList.toggle('label-faded', fade);
  });
}

function startDayNowTimer() {
  stopDayNowTimer();
  renderDayNowLine();
  dayNowTimer = setInterval(renderDayNowLine, 15000);
}

function _dayMinuteFromY(clientY) {
  const sheet = document.getElementById('day-sheet');
  if (!sheet) return null;
  const rect = sheet.getBoundingClientRect();
  if (!rect.height) return null;
  const minute = Math.floor((clientY - rect.top) * (24 * 60) / rect.height);
  if (minute < 0) return 0;
  if (minute >= 24 * 60) return 24 * 60 - 1;
  return minute;
}

function _dayMinuteLabel(minute) {
  return `${_pad2(Math.floor(minute / 60))}:${_pad2(minute % 60)}`;
}

function updateDayHoverLine(e) {
  const minute = _dayMinuteFromY(e.clientY);
  const line = document.getElementById('day-hover-line');
  const label = document.getElementById('day-hover-label');
  if (minute == null || !line) return;
  dayHoverMinute = minute;
  line.style.top = minute + 'px';
  line.style.display = '';
  if (label) label.textContent = _dayMinuteLabel(minute);
  _refreshDayLabelFade();
}

function hideDayHoverLine() {
  dayHoverMinute = null;
  const line = document.getElementById('day-hover-line');
  if (line) line.style.display = 'none';
  _refreshDayLabelFade();
}

function openScheduleAtPointer(e) {
  if (e.target.closest('[data-sched-detail]')) return;
  if (!e.target.closest('#day-sheet')) return;
  const minute = dayHoverMinute != null ? dayHoverMinute : _dayMinuteFromY(e.clientY);
  if (minute == null) return;
  openScheduleModal(null, { date: scheduleDay, time: _dayMinuteLabel(minute) });
}

function renderScheduleDay() {
  const wrap = document.getElementById('schedule-day-hours');
  if (!wrap) return;
  const day = _parseDayKey(scheduleDay);
  const label = document.getElementById('schedule-day-label');
  if (label) {
    label.textContent = day.toLocaleDateString([], {
      weekday: 'long', month: 'long', day: 'numeric', year: 'numeric',
    });
  }

  const dayStart = new Date(day.getFullYear(), day.getMonth(), day.getDate()).getTime();
  const dayEnd = new Date(day.getFullYear(), day.getMonth(), day.getDate() + 1).getTime();
  const occurrences = _schedOccurrencesBetween(dayStart, dayEnd)
    .map(o => ({ ...o, schedule: _scheduleById(o.schedule_id) }))
    .filter(o => o.schedule && (_showPausedSchedules() || (o.enabled !== false && _schedEnabled(o.schedule))));

  const byMinute = new Map();
  for (const o of occurrences) {
    const dt = new Date(Number(o.time) * 1000);
    const minute = dt.getHours() * 60 + dt.getMinutes();
    if (!byMinute.has(minute)) byMinute.set(minute, []);
    byMinute.get(minute).push(o);
  }

  dayHoverMinute = null;
  dayNowMinute = null;
  const hourLines = [];
  for (let h = 0; h < 24; h++) {
    hourLines.push(`<div class="day-hour-line" data-minute="${h * 60}" style="top:${h * DAY_HOUR_PX}px">
      <span class="day-hour-label">${_pad2(h)}:00</span>
    </div>`);
    if (h < 23) {
      hourLines.push(`<div class="day-half-line" style="top:${h * DAY_HOUR_PX + DAY_HOUR_PX / 2}px"></div>`);
    }
  }

  const eventHTML = [...byMinute.entries()]
    .sort((a, b) => a[0] - b[0])
    .map(([minute, list]) => `<div class="day-event-group" style="top:${minute}px">
      ${list.map(o => _schedChip({
        ts: Number(o.time) * 1000,
        count: 1,
        schedule: o.schedule,
        enabled: o.enabled !== false && _schedEnabled(o.schedule),
      })).join('')}
    </div>`).join('');

  wrap.innerHTML = `
    <div class="day-sheet" id="day-sheet" data-date="${esc(scheduleDay)}"
         style="height:${24 * DAY_HOUR_PX}px" role="presentation">
      ${hourLines.join('')}
      ${eventHTML}
      <div class="day-now-line" id="day-now-line" style="display:none">
        <span class="day-now-label" id="day-now-label"></span>
      </div>
      <div class="day-hover-line" id="day-hover-line" style="display:none">
        <span class="day-hover-label" id="day-hover-label"></span>
      </div>
    </div>`;
  startDayNowTimer();
}

function renderScheduleList() {
  const wrap = document.getElementById('schedule-list');
  if (!wrap) return;
  if (!schedules.length) {
    wrap.innerHTML = '<div class="sched-note" style="padding:16px 0">No schedules yet. Use “New schedule” or click an empty hour in the calendar.</div>';
    return;
  }
  const rows = schedules.map(s => {
    const paused = !_schedEnabled(s);
    return `
      <tr class="${paused ? 'sched-paused' : ''}">
        <td>
          <span class="sched-name">${esc(s.name)}</span>
          <span class="sched-id">${esc(s.id)}</span>
        </td>
        <td class="sched-targets">${esc(_scheduleTargetsLabel(s))}</td>
        <td><span class="sched-action" title="${esc(_scheduleActionLabel(s))}">${esc(_scheduleActionLabel(s))}</span></td>
        <td>${esc(_scheduleCadence(s))}</td>
        <td>${_schedEnabled(s) ? esc(_fmtScheduleWhen(_schedNextRun(s))) : '<span class="sched-id">paused</span>'}</td>
        <td>${_schedStatusBadge(s)}</td>
        <td><label class="sched-switch"><input type="checkbox" data-sched-toggle="${esc(s.id)}"
              ${paused ? '' : 'checked'} aria-label="Enable ${esc(s.name)}"></label></td>
        <td>
          <div class="sched-actions">
            <button type="button" class="btn btn-ghost btn-sm" data-sched-run="${esc(s.id)}">Run now</button>
            <button type="button" class="btn btn-ghost btn-sm" data-sched-history="${esc(s.id)}">History</button>
            <button type="button" class="btn btn-ghost btn-sm" data-sched-edit="${esc(s.id)}">Edit</button>
            <button type="button" class="btn btn-ghost btn-sm" data-sched-delete="${esc(s.id)}">Delete</button>
          </div>
        </td>
      </tr>`;
  }).join('');
  wrap.innerHTML = `
    <table class="schedules-table">
      <thead><tr>
        <th>Name</th><th>Targets</th><th>Action</th><th>Cadence</th>
        <th>Next run</th><th>Last status</th><th>Enabled</th><th></th>
      </tr></thead>
      <tbody>${rows}</tbody>
    </table>`;
}

function scheduleMonthOffset(delta) {
  scheduleMonth = new Date(scheduleMonth.getFullYear(), scheduleMonth.getMonth() + delta, 1);
  loadScheduleOccurrences().then(renderScheduleViews);
}

function scheduleMonthToday() {
  const d = new Date();
  scheduleMonth = new Date(d.getFullYear(), d.getMonth(), 1);
  loadScheduleOccurrences().then(renderScheduleViews);
}

function showScheduleDay(dateStr, push = true) {
  scheduleDay = dateStr;
  scheduleView = 'month';
  if (push) history.pushState({ view: 'schedule-day', date: dateStr }, '', '/schedules/' + dateStr);
  showView('view-schedules');
  currentServerId = null;
  updateSidebarActive(null);
  document.title = 'Schedules — ServerHub';
  loadSchedules();
}

function showScheduleMonth(push = true) {
  scheduleDay = null;
  scheduleView = 'month';
  if (push) history.pushState({ view: 'schedules' }, '', '/schedules');
  if (document.getElementById('view-schedules').style.display === 'none') {
    showView('view-schedules');
    currentServerId = null;
    updateSidebarActive(null);
  }
  document.title = 'Schedules — ServerHub';
  loadScheduleOccurrences().then(renderScheduleViews);
}

async function fetchOccurrences(from, to) {
  const q = new URLSearchParams({ from, to });
  return (await apiFetch('/api/schedules/occurrences?' + q.toString())) || [];
}

// One shared cache of occurrence ranges; both the schedules page and the
// dashboard week read through it, so they cannot drift apart. It is cleared
// whenever schedules change (load, create, edit, toggle, delete).
const occurrenceCache = new Map();

function invalidateOccurrenceCache() {
  occurrenceCache.clear();
}

async function getOccurrences(from, to, force = false) {
  const key = from + '|' + to;
  if (!force && occurrenceCache.has(key)) return occurrenceCache.get(key);
  try {
    const entries = await fetchOccurrences(from, to);
    occurrenceCache.set(key, entries);
    return entries;
  } catch (e) {
    occurrenceCache.delete(key);
    return [];
  }
}

function _scheduleOccurrenceQuery() {
  if (scheduleDay) {
    const d = _parseDayKey(scheduleDay);
    const next = new Date(d.getFullYear(), d.getMonth(), d.getDate() + 1);
    return { from: _dayKey(d), to: _dayKey(next) };
  }
  const first = new Date(scheduleMonth.getFullYear(), scheduleMonth.getMonth(), 1);
  const next = new Date(scheduleMonth.getFullYear(), scheduleMonth.getMonth() + 1, 1);
  return { from: _dayKey(first), to: _dayKey(next) };
}

async function loadScheduleOccurrences(force = false) {
  const range = _scheduleOccurrenceQuery();
  scheduleOccurrences = await getOccurrences(range.from, range.to, force);
}

async function loadSchedules() {
  try {
    schedules = (await apiFetch('/api/schedules')) || [];
  } catch (e) {
    toast('Could not load schedules: ' + e.message, 'err');
    schedules = [];
  }
  invalidateOccurrenceCache();
  checkScheduleConfigErrors();
  await loadScheduleOccurrences(true);
  scheduleDataSig = JSON.stringify([schedules, scheduleOccurrences]);
  renderSchedules();
  loadDashboardWeek();
}

// schedules.conf entries that had to be skipped at load are non-fatal, but
// they are worth surfacing near the top of the Schedules page.
async function checkScheduleConfigErrors() {
  const el = document.getElementById('sched-config-warning');
  if (!el) return;
  let errors = [];
  try {
    const status = await apiFetch('/api/scheduler/status');
    errors = (status && status.errors) || [];
  } catch (_) {}
  el.style.display = errors.length ? '' : 'none';
  el.textContent = errors.join(' · ');
}

// Quiet refresh: keep calendar, list, and dashboard week current without
// user action. Re-renders only when the data actually changed, and never
// while a modal is open or the tab is hidden.
let scheduleAutoTimer = null;
let scheduleDataSig = '';

function _schedModalOpen() {
  return [...document.querySelectorAll('.modal-overlay')].some(el => el.style.display === 'flex');
}

async function refreshSchedulesQuietly() {
  if (document.hidden || _schedModalOpen()) return;
  const schedView = document.getElementById('view-schedules');
  const dashView = document.getElementById('view-dashboard');
  const onSchedules = !!schedView && schedView.style.display !== 'none';
  const onDashboard = !!dashView && dashView.style.display !== 'none';
  if (!onSchedules && !onDashboard) return;
  let next;
  try {
    next = (await apiFetch('/api/schedules')) || [];
  } catch (_) { return; }
  let occ = [];
  if (onSchedules) {
    const range = _scheduleOccurrenceQuery();
    occ = await getOccurrences(range.from, range.to, true);
    scheduleOccurrences = occ;
  }
  const sig = JSON.stringify([next, occ]);
  if (sig === scheduleDataSig) return;
  scheduleDataSig = sig;
  schedules = next;
  renderSchedules();
  loadDashboardWeek();
}

function startScheduleAutoRefresh() {
  if (scheduleAutoTimer) return;
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) refreshSchedulesQuietly();
  });
  scheduleAutoTimer = setInterval(refreshSchedulesQuietly, 60000);
}

function renderSchedules() {
  renderScheduleViews();
  renderDashboardWeek();
}

async function runScheduleNow(id, btn = null) {
  const s = _scheduleById(id);
  const label = s && s.name ? s.name : id;
  if (btn) { btn.disabled = true; btn.textContent = 'Running…'; }
  toast(`Run started for “${label}” — results appear in history`, 'ok');
  try {
    await apiFetch(`/api/schedules/${encodeURIComponent(id)}/run`, { method: 'POST' });
    // The server dispatches and returns: starts land within seconds. Pull
    // fresh data twice so history/last status shows up without a page load.
    setTimeout(() => { loadSchedules().catch(() => {}); }, 3000);
    setTimeout(() => { loadSchedules().catch(() => {}); }, 10000);
  } catch (e) {
    toast('Could not run schedule: ' + e.message, 'err');
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = 'Run now'; }
  }
}

async function toggleSchedule(id, enabled) {
  try {
    const updated = await apiFetch(`/api/schedules/${encodeURIComponent(id)}`, {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled }),
    });
    const idx = schedules.findIndex(x => x.id === id);
    if (idx !== -1) schedules[idx] = updated;
    invalidateOccurrenceCache();
    await loadScheduleOccurrences(true);
    scheduleDataSig = JSON.stringify([schedules, scheduleOccurrences]);
    renderSchedules();
    loadDashboardWeek();
    if (scheduleDetailId === id) renderScheduleDetail();
    toast(`Schedule ${enabled ? 'resumed' : 'paused'}`, 'ok');
  } catch (e) {
    toast('Could not update schedule: ' + e.message, 'err');
    renderSchedules();
  }
}

async function deleteSchedule(id, confirmed = false) {
  const s = _scheduleById(id);
  if (!confirmed) {
    const ok = await confirmDialog({
      title: 'Delete schedule',
      message: `Delete schedule "${s ? s.name : id}"?`,
      confirmLabel: 'Delete schedule',
    });
    if (!ok) return;
  }
  try {
    await apiFetch(`/api/schedules/${encodeURIComponent(id)}`, { method: 'DELETE' });
    schedules = schedules.filter(x => x.id !== id);
    invalidateOccurrenceCache();
    await loadScheduleOccurrences(true);
    scheduleDataSig = JSON.stringify([schedules, scheduleOccurrences]);
    renderSchedules();
    loadDashboardWeek();
    toast('Schedule deleted', 'ok');
  } catch (e) {
    toast('Could not delete schedule: ' + e.message, 'err');
  }
}

function openScheduleDetail(id, occurrence = null) {
  const s = _scheduleById(id);
  if (!s) return;
  scheduleDetailId = id;
  scheduleDetailOccurrence = occurrence ? Number(occurrence) : null;
  renderScheduleDetail();
  document.getElementById('schedule-detail-modal').style.display = 'flex';
  document.addEventListener('keydown', scheduleDetailKeyHandler);
}

function closeScheduleDetail() {
  scheduleDetailId = null;
  scheduleDetailOccurrence = null;
  document.getElementById('schedule-detail-modal').style.display = 'none';
  document.removeEventListener('keydown', scheduleDetailKeyHandler);
}

function scheduleDetailKeyHandler(e) {
  if (e.key === 'Escape') closeScheduleDetail();
}

function scheduleDetailOverlayClick(e) {
  if (e.target === document.getElementById('schedule-detail-modal')) closeScheduleDetail();
}

function renderScheduleDetail() {
  const s = _scheduleById(scheduleDetailId);
  if (!s) { closeScheduleDetail(); return; }
  document.getElementById('schedule-detail-title').textContent = s.name;
  const pauseBtn = document.getElementById('sched-detail-pause');
  if (pauseBtn) pauseBtn.textContent = _schedEnabled(s) ? 'Pause' : 'Resume';
  const runs = (s.next_runs || []).slice(0, 5);
  const body = document.getElementById('schedule-detail-body');
  body.innerHTML = `
    <div class="sched-detail-grid">
      <div class="sched-detail-label">Status</div>
      <div class="sched-detail-value">
        <span class="sched-detail-status ${esc(_schedStatusClass(s))}">${esc(s.running ? 'running' : (s.last_status || 'never run'))}</span>
        ${s.last_summary ? ' · ' + esc(s.last_summary) : ''}
      </div>
      <div class="sched-detail-label">Targets</div>
      <div class="sched-detail-value">${esc(_scheduleTargetsLabel(s))}</div>
      <div class="sched-detail-label">Action</div>
      <div class="sched-detail-value">${esc(_scheduleActionLabel(s))}</div>
      <div class="sched-detail-label">Cadence</div>
      <div class="sched-detail-value">${esc(_scheduleCadence(s))}</div>
      ${scheduleDetailOccurrence ? `
      <div class="sched-detail-label">Selected run</div>
      <div class="sched-detail-value">${esc(_fmtScheduleWhen(scheduleDetailOccurrence))}</div>` : ''}
      <div class="sched-detail-label">Next runs</div>
      <div class="sched-detail-value sched-detail-runs">
        ${runs.length ? runs.map(ts => `<span class="sched-preview-run">${esc(_fmtScheduleWhen(ts))}</span>`).join('') : '—'}
      </div>
      <div class="sched-detail-label">Last run</div>
      <div class="sched-detail-value">${esc(_fmtScheduleWhen(s.last_run))}</div>
      ${s.ends_at ? `
      <div class="sched-detail-label">Ends</div>
      <div class="sched-detail-value">${esc(_fmtScheduleWhen(s.ends_at))}</div>` : ''}
      <div class="sched-detail-label">Behaviour</div>
      <div class="sched-detail-value">overlap: ${esc(s.overlap || 'skip')} · timeout: ${esc(s.timeout || 'none')}</div>
    </div>
    <div style="margin-top:14px;display:flex;justify-content:flex-end">
      <button type="button" class="btn btn-ghost btn-sm" onclick="duplicateSchedule()">Duplicate</button>
    </div>`;
}

async function scheduleDetailAction(action) {
  const s = _scheduleById(scheduleDetailId);
  if (!s) return;
  if (action === 'run') {
    await runScheduleNow(s.id);
  } else if (action === 'toggle') {
    await toggleSchedule(s.id, !_schedEnabled(s));
  } else if (action === 'history') {
    closeScheduleDetail();
    openScheduleHistory(s.id);
  } else if (action === 'edit') {
    closeScheduleDetail();
    openScheduleModal(s.id);
  } else if (action === 'delete') {
    if (scheduleDetailOccurrence) {
      openEventDeleteModal();
    } else {
      closeScheduleDetail();
      deleteSchedule(s.id);
    }
  }
}

function openEventDeleteModal() {
  document.getElementById('event-delete-modal').style.display = 'flex';
  document.addEventListener('keydown', eventDeleteKeyHandler);
}

function closeEventDeleteModal() {
  document.getElementById('event-delete-modal').style.display = 'none';
  document.removeEventListener('keydown', eventDeleteKeyHandler);
}

function eventDeleteOverlayClick(e) {
  if (e.target === document.getElementById('event-delete-modal')) closeEventDeleteModal();
}

function eventDeleteKeyHandler(e) {
  if (e.key === 'Escape') closeEventDeleteModal();
}

async function deleteScheduleOccurrence(future) {
  const id = scheduleDetailId;
  const ts = scheduleDetailOccurrence;
  if (!id || !ts) return;
  try {
    await apiFetch(`/api/schedules/${encodeURIComponent(id)}/delete-occurrence`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ time: ts, future: !!future }),
    });
    closeEventDeleteModal();
    closeScheduleDetail();
    await loadSchedules();
    toast(future ? 'Future events deleted' : 'Event deleted', 'ok');
  } catch (e) {
    toast('Could not delete event: ' + e.message, 'err');
  }
}

function duplicateSchedule() {
  const s = _scheduleById(scheduleDetailId);
  if (!s) return;
  closeScheduleDetail();
  openScheduleModal(null, { copy: s });
}

// ── Builder ──────────────────────────────────────────────────────────────────

function scheduleModalMsg(text, cls) {
  const el = document.getElementById('schedule-modal-msg');
  if (!el) return;
  el.textContent = text || '';
  el.className = 'pw-msg' + (cls ? ' ' + cls : '');
}

function _splitList(s) {
  return String(s || '').split(',').map(v => v.trim()).filter(Boolean);
}

function allScheduleTargets() {
  const out = [];
  if (localMachine && localMachine.id) {
    out.push({
      id: '__local__',
      name: localMachine.name || localHostname || 'This machine',
      tags: localMachine.tags || [],
      quick_actions: localMachine.quick_actions || [],
      online: true,
      tmux: localMachine.tmux !== false,
      local: true,
    });
  }
  for (const s of servers || []) {
    out.push({
      id: s.id,
      name: s.name || s.id,
      tags: s.tags || [],
      quick_actions: s.quick_actions || [],
      online: !!s.online,
      tmux: true,
      local: false,
    });
  }
  return out;
}

function _schedTargetPool() { return allScheduleTargets(); }

function selectedScheduleTargetIDs() {
  const list = document.getElementById('sched-target-list');
  if (!list) return [];
  return [...list.querySelectorAll('input[type="checkbox"]:checked')].map(el => el.value);
}

function _schedTagList() {
  return _splitList(document.getElementById('sched-tags').value);
}

function refreshScheduleTargets(s) {
  const list = document.getElementById('sched-target-list');
  if (!list) return;
  const pool = _schedTargetPool();
  const selected = new Set((s && s.targets) || []);
  list.innerHTML = pool.map(t => {
    const disabled = t.local && !t.tmux;
    return `<label class="sched-target${disabled ? ' disabled' : ''}"
              data-target-name="${esc((t.name + ' ' + t.id + ' ' + (t.tags || []).join(' ')).toLowerCase())}"
              title="${disabled ? 'Local detached jobs require tmux' : esc(t.id)}">
      <input type="checkbox" value="${esc(t.id)}" ${selected.has(t.id) ? 'checked' : ''} ${disabled ? 'disabled' : ''}>
      <span class="sched-target-name">${esc(t.name)}</span>
      <span class="sched-target-id">${esc(t.id === '__local__' ? 'local' : t.id)}</span>
    </label>`;
  }).join('');
  const search = document.getElementById('sched-target-search');
  if (search) search.value = '';

  const tagSet = new Set();
  pool.forEach(t => (t.tags || []).forEach(tag => tagSet.add(tag)));
  const tagOptions = document.getElementById('sched-tag-options');
  if (tagOptions) {
    tagOptions.innerHTML = [...tagSet].sort().map(tag => `<option value="${esc(tag)}"></option>`).join('');
  }
  updateScheduleTargetSummary();
  refreshScheduleQuickActions((s && s.quick_action) || '');
}

function filterScheduleTargets() {
  const needle = String(document.getElementById('sched-target-search').value || '').trim().toLowerCase();
  document.querySelectorAll('#sched-target-list .sched-target').forEach(el => {
    el.style.display = !needle || (el.dataset.targetName || '').includes(needle) ? '' : 'none';
  });
}

function updateScheduleTargetSummary() {
  const ids = selectedScheduleTargetIDs();
  const tags = _schedTagList();
  const pool = _schedTargetPool();
  const tagMatches = new Set();
  const missing = [];
  for (const tag of tags) {
    let found = false;
    for (const t of pool) {
      if ((t.tags || []).includes(tag)) { tagMatches.add(t.id); found = true; }
    }
    if (!found) missing.push(tag);
  }
  const total = new Set([...ids, ...tagMatches]).size;
  const summary = document.getElementById('sched-target-summary');
  if (summary) {
    summary.textContent = total
      ? `${total} target${total === 1 ? '' : 's'} at run time`
      : 'No targets selected';
  }
  const warning = document.getElementById('sched-tag-warning');
  if (warning) warning.textContent = missing.length ? 'No current server has tag: ' + missing.join(', ') : '';
}

function refreshScheduleQuickActions(selectedId) {
  const select = document.getElementById('sched-quick-action');
  if (!select) return;
  const pool = _schedTargetPool();
  const ids = new Set(selectedScheduleTargetIDs());
  const tags = _schedTagList();
  const considered = pool.filter(t => ids.has(t.id) || tags.some(tag => (t.tags || []).includes(tag)));
  const scope = considered.length ? considered : pool;
  const total = considered.length;

  const map = new Map();
  for (const t of scope) {
    for (const qa of t.quick_actions || []) {
      if (!qa.id) continue;
      if (!map.has(qa.id)) map.set(qa.id, { id: qa.id, label: qa.label || qa.id, servers: new Set() });
      map.get(qa.id).servers.add(t.id);
    }
  }
  const wanted = selectedId || select.value;
  const options = [...map.values()].sort((a, b) => a.label.localeCompare(b.label));
  let html = '';
  for (const opt of options) {
    const count = opt.servers.size;
    const suffix = total ? ` — ${opt.label} (${count}/${total})` : ` — ${opt.label}`;
    html += `<option value="${esc(opt.id)}" label="${esc(suffix)}">${esc(opt.id)}</option>`;
  }
  if (wanted && !map.has(wanted)) {
    html += `<option value="${esc(wanted)}">${esc(wanted)} (not on selected targets)</option>`;
  }
  if (!html) html = '<option value="">No quick actions available</option>';
  select.innerHTML = html;
  if (wanted) select.value = wanted;

  const warning = document.getElementById('sched-qa-warning');
  if (warning) {
    if (!wanted) {
      warning.textContent = total ? 'Pick an action that exists on every target.' : '';
    } else if (map.has(wanted) && total && map.get(wanted).servers.size < total) {
      const missing = considered.filter(t => !map.get(wanted).servers.has(t.id)).map(t => t.name);
      warning.textContent = `Not available on: ${missing.join(', ')}`;
    } else {
      warning.textContent = '';
    }
  }
}

function _syncSegmentedButtons(id, value) {
  const group = document.getElementById(id);
  if (!group) return;
  group.querySelectorAll('[data-seg]').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.seg === value);
  });
}

function setScheduleKind(value) {
  document.getElementById('sched-kind').value = value;
  _syncSegmentedButtons('sched-kind-seg', value);
  schedKindChanged();
}

function setScheduleOverlap(value) {
  document.getElementById('sched-overlap').value = value;
  _syncSegmentedButtons('sched-overlap-seg', value);
  scheduleFormChanged();
}

function schedKindChanged() {
  const kind = document.getElementById('sched-kind').value;
  _syncSegmentedButtons('sched-kind-seg', kind);
  document.getElementById('sched-qa-group').style.display = kind === 'quick_action' ? '' : 'none';
  document.getElementById('sched-command-group').style.display = kind === 'command' ? '' : 'none';
  scheduleFormChanged();
}

function _repeatForSchedule(s) {
  if (!s) return 'daily';
  if (s.date) return 'once';
  if (s.days && s.days.length) {
    const weekdays = ['mon', 'tue', 'wed', 'thu', 'fri'];
    if (s.days.length === weekdays.length && weekdays.every(d => s.days.includes(d))) return 'weekdays';
    return 'weekly';
  }
  if (s.cron) return 'cron';
  const ms = _parseDurationMs(s.every);
  if (s.at && ms >= 86400000 && ms % 86400000 === 0) return 'daily';
  if (ms >= 3600000 && ms % 3600000 === 0) return 'hours';
  if (ms >= 60000 && ms % 60000 === 0) return 'minutes';
  if (ms > 0) return 'custom';
  if (s.at) return 'daily';
  return 'daily';
}

function schedRepeatChanged() {
  const repeat = document.getElementById('sched-repeat').value;
  const show = (id, yes) => {
    const el = document.getElementById(id);
    if (el) el.style.display = yes ? '' : 'none';
  };
  show('sched-once-group', repeat === 'once');
  show('sched-interval-group', repeat === 'minutes' || repeat === 'hours');
  show('sched-daily-group', repeat === 'daily');
  show('sched-days-group', repeat === 'weekdays' || repeat === 'weekly');
  show('sched-custom-group', repeat === 'custom');
  show('sched-cron-group', repeat === 'cron');
  show('sched-time-group', ['once', 'daily', 'weekdays', 'weekly'].includes(repeat));

  if (repeat === 'weekdays') {
    document.querySelectorAll('.sched-day').forEach(btn => {
      btn.classList.toggle('active', ['mon', 'tue', 'wed', 'thu', 'fri'].includes(btn.dataset.schedDay));
    });
  } else if (repeat === 'weekly') {
    const any = [...document.querySelectorAll('.sched-day.active')].length;
    if (!any) {
      const today = ['sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat'][new Date().getDay()];
      const btn = document.querySelector(`.sched-day[data-sched-day="${today}"]`);
      if (btn) btn.classList.add('active');
    }
  }
  scheduleFormChanged();
}

function toggleSchedDay(btn) {
  btn.classList.toggle('active');
  scheduleFormChanged();
}

function selectedSchedDays() {
  return [...document.querySelectorAll('.sched-day.active')].map(btn => btn.dataset.schedDay);
}

function scheduleTagsChanged() {
  refreshScheduleQuickActions(document.getElementById('sched-quick-action').value);
  scheduleFormChanged();
}

function scheduleFormChanged() {
  updateScheduleTargetSummary();
  schedulePreviewSoon();
}

function schedulePreviewSoon() {
  clearTimeout(schedulePreviewTimer);
  schedulePreviewTimer = setTimeout(refreshSchedulePreview, 250);
}

async function refreshSchedulePreview() {
  const list = document.getElementById('sched-preview-list');
  const warnings = document.getElementById('sched-preview-warnings');
  if (!list) return;
  const repeat = document.getElementById('sched-repeat').value;
  if (repeat === 'weekly' && selectedSchedDays().length === 0) {
    list.innerHTML = '<span class="sched-note">Choose at least one day.</span>';
    warnings.textContent = '';
    return;
  }
  if (repeat === 'cron' && !document.getElementById('sched-cron').value.trim()) {
    list.innerHTML = '<span class="sched-note">Enter a cron expression.</span>';
    warnings.textContent = '';
    return;
  }
  if (repeat === 'custom' && !document.getElementById('sched-custom-every').value.trim()) {
    list.innerHTML = '<span class="sched-note">Enter a duration such as 10s or 90m.</span>';
    warnings.textContent = '';
    return;
  }
  const seq = ++schedulePreviewSeq;
  const draft = buildScheduleDraft();
  try {
    const preview = await apiFetch('/api/schedules/preview', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(draft),
    });
    if (seq !== schedulePreviewSeq) return;
    const runs = preview.next_runs || [];
    list.innerHTML = runs.length
      ? runs.map(ts => `<span class="sched-preview-run">${esc(_fmtScheduleWhen(ts))}</span>`).join('')
      : '<span class="sched-note">No future runs.</span>';
    const notes = [...(preview.warnings || [])];
    if ((preview.missing || []).length) notes.push('Missing: ' + preview.missing.join(', '));
    warnings.textContent = notes.join(' · ');
  } catch (e) {
    if (seq !== schedulePreviewSeq) return;
    list.innerHTML = '<span class="sched-note">—</span>';
    warnings.textContent = e.message;
  }
}

function buildScheduleDraft() {
  const val = id => {
    const el = document.getElementById(id);
    return el ? String(el.value || '').trim() : '';
  };
  const repeat = val('sched-repeat') || 'daily';
  const time = _schedTimeValue(val('sched-time'));
  let date = '';
  let days = [];
  let every = '';
  let at = '';
  let cron = '';

  if (repeat === 'once') {
    date = _schedDateValue(val('sched-date'));
    at = time;
  } else if (repeat === 'minutes') {
    every = (parseInt(val('sched-interval'), 10) || 1) + 'm';
  } else if (repeat === 'hours') {
    every = (parseInt(val('sched-interval'), 10) || 1) + 'h';
  } else if (repeat === 'daily') {
    every = ((parseInt(val('sched-daily-every'), 10) || 1) * 24) + 'h';
    at = time;
  } else if (repeat === 'weekdays') {
    days = ['mon', 'tue', 'wed', 'thu', 'fri'];
    at = time;
  } else if (repeat === 'weekly') {
    days = selectedSchedDays();
    at = time;
  } else if (repeat === 'custom') {
    every = val('sched-custom-every');
  } else if (repeat === 'cron') {
    cron = val('sched-cron');
  }

  const kind = val('sched-kind') || 'quick_action';
  return {
    id: scheduleEditId || '',
    name: val('sched-name'),
    enabled: document.getElementById('sched-enabled').checked,
    targets: selectedScheduleTargetIDs(),
    tags: _schedTagList(),
    quick_action: kind === 'quick_action' ? val('sched-quick-action') : '',
    command: kind === 'command' ? val('sched-command') : '',
    date,
    days,
    every,
    at,
    cron,
    overlap: val('sched-overlap') || 'skip',
    timeout: val('sched-timeout'),
  };
}

function openScheduleModal(id = null, opts = {}) {
  const found = id ? _scheduleById(id) : null;
  const s = found || opts.copy || null;
  scheduleEditId = found ? found.id : null;

  document.getElementById('schedule-modal-title').textContent = scheduleEditId ? 'Edit Schedule' : 'New Schedule';
  document.getElementById('schedule-modal-save').textContent = scheduleEditId ? 'Save Changes' : 'Create schedule';
  document.getElementById('schedule-modal-delete').style.display = scheduleEditId ? '' : 'none';

  document.getElementById('sched-name').value = s ? (opts.copy ? s.name + ' copy' : s.name) : '';
  document.getElementById('sched-enabled').checked = !s || s.enabled !== false;
  document.getElementById('sched-tags').value = s ? (s.tags || []).join(', ') : '';
  document.getElementById('sched-kind').value = s && s.command && !s.quick_action ? 'command' : 'quick_action';
  document.getElementById('sched-command').value = s ? (s.command || '') : '';
  document.getElementById('sched-overlap').value = s ? (s.overlap || 'skip') : 'skip';
  _syncSegmentedButtons('sched-kind-seg', document.getElementById('sched-kind').value);
  _syncSegmentedButtons('sched-overlap-seg', document.getElementById('sched-overlap').value);
  document.getElementById('sched-timeout').value = s ? (s.timeout || '') : '';
  document.getElementById('sched-cron').value = s ? (s.cron || '') : '';
  document.getElementById('sched-time').value = _schedTimeValue(s ? s.at : (opts.time || '03:00'));
  document.getElementById('sched-date').value = _schedDateValue(s ? s.date : (opts.date || _schedTodayKey()));

  const repeat = opts.date && !s ? 'once' : _repeatForSchedule(s);
  document.getElementById('sched-repeat').value = repeat;
  const ms = s ? _parseDurationMs(s.every) : 0;
  if (repeat === 'minutes') {
    document.getElementById('sched-interval').value = Math.max(1, Math.round(ms / 60000));
  } else if (repeat === 'hours') {
    document.getElementById('sched-interval').value = Math.max(1, Math.round(ms / 3600000));
  } else {
    document.getElementById('sched-interval').value = 1;
  }
  document.getElementById('sched-daily-every').value = Math.max(1, Math.round(ms / 86400000) || 1);
  document.getElementById('sched-custom-every').value = s ? (s.every || '') : '';

  document.querySelectorAll('.sched-day').forEach(btn => {
    btn.classList.toggle('active', !!s && (s.days || []).includes(btn.dataset.schedDay));
  });

  refreshScheduleTargets(s);
  document.getElementById('sched-quick-action').value = s ? (s.quick_action || '') : '';
  refreshScheduleQuickActions(s ? (s.quick_action || '') : '');

  schedRepeatChanged();
  schedKindChanged();
  scheduleModalMsg('');
  document.getElementById('schedule-modal').style.display = 'flex';
  document.getElementById('sched-name').focus();
  document.addEventListener('keydown', scheduleModalKeyHandler);
  schedulePreviewSoon();
}

function closeScheduleModal() {
  document.getElementById('schedule-modal').style.display = 'none';
  document.removeEventListener('keydown', scheduleModalKeyHandler);
  scheduleEditId = null;
  clearTimeout(schedulePreviewTimer);
}

function scheduleModalKeyHandler(e) {
  if (e.key === 'Escape') closeScheduleModal();
  if (e.key === 'Enter' && e.target && e.target.id === 'sched-name') {
    e.preventDefault();
    saveSchedule();
  }
}

function scheduleOverlayClick(e) {
  if (e.target === document.getElementById('schedule-modal')) closeScheduleModal();
}

async function saveSchedule() {
  const draft = buildScheduleDraft();
  if (!draft.name) { scheduleModalMsg('Name is required', 'err'); return; }
  if (!draft.targets.length && !draft.tags.length) { scheduleModalMsg('Choose a server or add a tag', 'err'); return; }
  if (draft.quick_action === '' && draft.command === '') {
    scheduleModalMsg('Choose a quick action or enter a command', 'err');
    return;
  }
  const repeat = document.getElementById('sched-repeat').value;
  if (repeat === 'weekly' && !draft.days.length) {
    scheduleModalMsg('Choose at least one day', 'err');
    return;
  }
  if (repeat === 'cron' && !draft.cron) {
    scheduleModalMsg('Enter a cron expression', 'err');
    return;
  }
  if (repeat === 'custom' && !draft.every) {
    scheduleModalMsg('Enter a custom duration', 'err');
    return;
  }
  if (!draft.date && !draft.days.length && !draft.every && !draft.cron) {
    scheduleModalMsg('Choose a cadence', 'err');
    return;
  }

  const btn = document.getElementById('schedule-modal-save');
  btn.disabled = true;
  try {
    if (scheduleEditId) {
      await apiFetch(`/api/schedules/${encodeURIComponent(scheduleEditId)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
      toast('Schedule updated', 'ok');
    } else {
      await apiFetch('/api/schedules', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(draft),
      });
      toast('Schedule created', 'ok');
    }
    closeScheduleModal();
    await loadSchedules();
  } catch (e) {
    scheduleModalMsg(e.message, 'err');
  } finally {
    btn.disabled = false;
  }
}

async function deleteScheduleFromModal() {
  if (!scheduleEditId) return;
  const id = scheduleEditId;
  const s = _scheduleById(id);
  const ok = await confirmDialog({
    title: 'Delete schedule',
    message: `Delete schedule "${s ? s.name : id}"?`,
    confirmLabel: 'Delete schedule',
  });
  if (!ok) return;
  closeScheduleModal();
  deleteSchedule(id, true);
}

// ── History ──────────────────────────────────────────────────────────────────

function openScheduleHistory(id) {
  const s = _scheduleById(id);
  if (!s) return;
  document.getElementById('schedule-history-title').textContent = `Run history — ${s.name}`;
  document.getElementById('schedule-history-body').innerHTML = _historyHTML(s);
  document.getElementById('schedule-history-modal').style.display = 'flex';
  document.addEventListener('keydown', scheduleHistoryKeyHandler);
}

function _historyHTML(s) {
  if (!s.history || !s.history.length) {
    return '<div class="history-empty">No runs recorded yet.</div>';
  }
  return '<div class="history-list">' + s.history.map(run => {
    const when = new Date((run.time || 0) * 1000).toLocaleString();
    const targets = (run.targets || []).map(t => {
      const err = t.error ? ' — ' + esc(t.error) : '';
      const job = t.job_id
        ? ` <button type="button" class="sched-job-link" data-sched-job-server="${esc(t.server_id)}" data-sched-job-id="${esc(t.job_id)}">view output</button>`
        : '';
      return `<span class="history-target">${esc(_serverLabel(t.server_id || '—'))}: ${esc(t.status)}${err}${job}</span>`;
    }).join('');
    return `
      <div class="history-run">
        <div class="history-run-top">
          <span class="history-run-time">${esc(when)}</span>
          <span class="sched-badge ${esc(run.status)}">${esc(run.status)}</span>
          <span class="history-run-summary">${esc(run.trigger)}${run.summary ? ' · ' + esc(run.summary) : ''}</span>
        </div>
        <div class="history-targets">${targets}</div>
      </div>`;
  }).join('') + '</div>';
}

function closeScheduleHistory() {
  document.getElementById('schedule-history-modal').style.display = 'none';
  document.removeEventListener('keydown', scheduleHistoryKeyHandler);
}

function scheduleHistoryKeyHandler(e) {
  if (e.key === 'Escape') closeScheduleHistory();
}

function scheduleHistoryOverlayClick(e) {
  if (e.target === document.getElementById('schedule-history-modal')) closeScheduleHistory();
}

// ── Delegated click handlers ──────────────────────────────────────────────────
//
// Data-derived buttons never carry inline `onclick` strings (XSS containment):
// markup only declares data-* attributes; these listeners resolve the action.

function attachDelegatedHandlers() {
  const on = (containerId, selector, fn) => {
    const el = document.getElementById(containerId);
    if (!el) return;
    el.addEventListener('click', e => {
      const target = e.target.closest(selector);
      if (target) fn(target, e);
    });
  };

  on('server-list', '.server-nav-item', (el, e) => {
    if (e.target.closest('[data-star]')) return;
    showDetail(el.dataset.id);
  });
  on('server-cards', '.server-card', (el, e) => {
    if (e.target.closest('[data-star]')) return;
    showDetail(el.dataset.id);
  });
  on('favorite-cards', '.server-card', (el, e) => {
    if (e.target.closest('[data-star]')) return;
    showDetail(el.dataset.id);
  });
  on('server-list', '[data-star]', el => toggleFavorite(el.dataset.star));
  on('server-cards', '[data-star]', el => toggleFavorite(el.dataset.star));
  on('favorite-cards', '[data-star]', el => toggleFavorite(el.dataset.star));

  const appClick = (el, e) => {
    const card = el.closest('.app-card');
    if (!card || e.target.closest('a, button')) return;
    openAppWindow(card.dataset.appId);
  };
  for (const containerId of ['dashboard-services', 'apps-grid']) {
    on(containerId, '[data-app-star]', el => {
      const app = _appByID(el.dataset.appStar);
      if (app) toggleAppFavorite(app.id, !app.favorite);
    });
    on(containerId, '[data-app-edit]', el => openAppModal(el.dataset.appEdit));
    on(containerId, '[data-app-delete]', el => deleteApp(el.dataset.appDelete));
    on(containerId, '.app-card', appClick);
  }

  on('dashboard-qa-list', '[data-dash-qa-run]', el =>
    toggleDashboardQA(el.dataset.dashQaRun, parseInt(el.dataset.qaIdx, 10) || 0));
  on('dashboard-qa-list', '[data-dash-qa-open]', el => showDetail(el.dataset.dashQaOpen));

  on('containers-body', '[data-container-action]', el =>
    containerAction(el.dataset.server, el.dataset.name, el.dataset.containerAction));
  on('containers-body', '[data-container-logs]', el =>
    openContainerLogs(el.dataset.server, el.dataset.containerLogs));

  on('qa-list', '[data-qa-run]', el =>
    toggleQA(el.dataset.server, parseInt(el.dataset.qaIdx, 10) || 0));
  on('qa-list', '[data-qa-edit]', el =>
    openQAModal(parseInt(el.dataset.qaEdit, 10)));

  on('jobs-list', '[data-job-observe]', el =>
    observeJob(el.dataset.jobServer, el.dataset.jobObserve));
  on('jobs-list', '[data-job-stop]', el =>
    jobAction(el.dataset.jobServer, el.dataset.jobStop, 'stop'));
  on('jobs-list', '[data-job-dismiss]', el =>
    dismissJob(el.dataset.jobServer, el.dataset.jobDismiss));

  on('schedule-calendar', '[data-sched-detail]', el => openScheduleDetail(el.dataset.schedDetail, el.dataset.schedOccurrence));
  on('schedule-calendar', '.cal-cell[data-date]', (el, e) => {
    if (e.target.closest('[data-sched-detail]')) return;
    showScheduleDay(el.dataset.date);
  });
  on('dashboard-week', '[data-sched-detail]', el => openScheduleDetail(el.dataset.schedDetail, el.dataset.schedOccurrence));
  on('dashboard-week', '.week-day', (el, e) => {
    if (e.target.closest('[data-sched-detail]')) return;
    showScheduleDay(el.dataset.date);
  });
  on('schedule-day-hours', '[data-sched-detail]', el => openScheduleDetail(el.dataset.schedDetail, el.dataset.schedOccurrence));
  on('schedule-list', '[data-sched-run]', el => runScheduleNow(el.dataset.schedRun, el));
  on('schedule-list', '[data-sched-history]', el => openScheduleHistory(el.dataset.schedHistory));
  on('schedule-list', '[data-sched-edit]', el => openScheduleModal(el.dataset.schedEdit));
  on('schedule-list', '[data-sched-delete]', el => deleteSchedule(el.dataset.schedDelete));
  const dashOrder = document.getElementById('dashboard-order-list');
  if (dashOrder) {
    dashOrder.addEventListener('click', e => {
      const btn = e.target.closest('[data-dash-move]');
      if (!btn) return;
      const row = btn.closest('[data-dash-order]');
      if (row) moveDashboardSection(row.dataset.dashOrder, parseInt(btn.dataset.dashMove, 10));
    });
    dashOrder.addEventListener('dragstart', e => {
      const row = e.target.closest('[data-dash-order]');
      if (!row) return;
      dashboardDragId = row.dataset.dashOrder;
      e.dataTransfer.effectAllowed = 'move';
    });
    dashOrder.addEventListener('dragover', e => {
      e.preventDefault();
      const row = e.target.closest('[data-dash-order]');
      dashOrder.querySelectorAll('.dash-order-row').forEach(el => el.classList.remove('drag-over'));
      if (row) row.classList.add('drag-over');
    });
    dashOrder.addEventListener('dragleave', e => {
      const row = e.target.closest('[data-dash-order]');
      if (row) row.classList.remove('drag-over');
    });
    dashOrder.addEventListener('drop', e => {
      e.preventDefault();
      const row = e.target.closest('[data-dash-order]');
      dashOrder.querySelectorAll('.dash-order-row').forEach(el => el.classList.remove('drag-over'));
      if (!row || !dashboardDragId) return;
      const from = dashboardOrder.indexOf(dashboardDragId);
      const to = dashboardOrder.indexOf(row.dataset.dashOrder);
      if (from === -1 || to === -1 || from === to) return;
      const [item] = dashboardOrder.splice(from, 1);
      dashboardOrder.splice(to, 0, item);
      applyDashboardConfig(dashboardOrder, dashboardShowDays, dashboardHidden);
      _saveAppConfig();
    });
    dashOrder.addEventListener('change', e => {
      const cb = e.target.closest('[data-dash-show]');
      if (cb) setDashboardSectionVisible(cb.dataset.dashShow, cb.checked);
    });
    dashOrder.addEventListener('dragend', () => {
      dashboardDragId = null;
      dashOrder.querySelectorAll('.dash-order-row').forEach(el => el.classList.remove('drag-over'));
    });
  }

  const mediaGrid = document.getElementById('media-library-grid');
  if (mediaGrid) {
    mediaGrid.addEventListener('click', async e => {
      const card = e.target.closest('.media-card');
      if (!card) return;
      const item = mediaLibraryItems.find(x => x.name === card.dataset.mediaName);
      if (!item) return;
      if (e.target.closest('[data-media-select]')) {
        if (!mediaLibraryOnSelect) return;
        try {
          await mediaLibraryOnSelect(item);
          closeMediaLibrary();
        } catch (err) {
          mediaMsg(err.message, 'err');
        }
      } else if (e.target.closest('[data-media-refresh]')) {
        await refreshMediaItem(item);
      } else if (e.target.closest('[data-media-delete]')) {
        await deleteMediaItem(item);
      }
    });
  }

  const schedList = document.getElementById('schedule-list');
  if (schedList) {
    schedList.addEventListener('change', e => {
      const toggle = e.target.closest('[data-sched-toggle]');
      if (toggle) toggleSchedule(toggle.dataset.schedToggle, toggle.checked);
    });
  }
  const schedTargets = document.getElementById('sched-target-list');
  if (schedTargets) {
    schedTargets.addEventListener('change', () => {
      const current = document.getElementById('sched-quick-action').value;
      refreshScheduleQuickActions(current);
      scheduleFormChanged();
    });
  }
  const dayHours = document.getElementById('schedule-day-hours');
  if (dayHours) {
    dayHours.addEventListener('pointermove', updateDayHoverLine);
    dayHours.addEventListener('pointerleave', hideDayHoverLine);
    dayHours.addEventListener('click', openScheduleAtPointer);
  }
  on('schedule-history-body', '[data-sched-job-server]', el => {
    closeScheduleHistory();
    const serverId = el.dataset.schedJobServer;
    const jobId = el.dataset.schedJobId;
    showDetail(serverId);
    observeJob(serverId, jobId);
    const section = document.getElementById('jobs-section');
    if (section) setTimeout(() => section.scrollIntoView({ behavior: 'smooth', block: 'start' }), 150);
  });

  on('action-row', '[data-confirm-yes]', el => runServerAction(el.dataset.id, el.dataset.action));
  on('action-row', '[data-confirm-cancel]', el => cancelConfirm(el.dataset.id, el.dataset.action));
  on('action-row', '[data-act]', el => confirmAction(el.dataset.id, el.dataset.act));

  const qaForm = document.getElementById('qa-form-list');
  if (qaForm) {
    qaForm.addEventListener('change', e => {
      const select = e.target.closest('[data-qa-field="mode"]');
      if (select) qaRowModeChanged(select);
    });
  }
  const follow = document.getElementById('job-follow');
  if (follow) follow.addEventListener('change', e => setJobFollow(e.target.checked));
  const clear = document.getElementById('job-output-clear');
  if (clear) clear.addEventListener('click', clearJobOutput);
  const close = document.getElementById('job-output-close');
  if (close) close.addEventListener('click', closeJobObserver);

  const clogFollow = document.getElementById('container-log-follow');
  if (clogFollow) clogFollow.addEventListener('change', e => setContainerLogFollow(e.target.checked));
  const clogClear = document.getElementById('container-log-clear');
  if (clogClear) clogClear.addEventListener('click', clearContainerLogs);
  const clogClose = document.getElementById('container-log-close');
  if (clogClose) clogClose.addEventListener('click', closeContainerLogs);

  const probeRun = document.getElementById('probe-run');
  if (probeRun) probeRun.addEventListener('click', runProbe);
  const probeType = document.getElementById('probe-type');
  if (probeType) probeType.addEventListener('change', probeTypeChanged);

  const appSearchEl = document.getElementById('app-search');
  if (appSearchEl) {
    appSearchEl.addEventListener('input', e => {
      appSearch = e.target.value;
      renderApps();
    });
  }

  const search = document.getElementById('global-search');
  if (search) {
    search.addEventListener('input', e => applySearch(e.target.value));
    search.addEventListener('keydown', e => {
      if (e.key === 'Enter') {
        e.preventDefault();
        openFirstSearchResult();
      } else if (e.key === 'Escape') {
        search.value = '';
        applySearch('');
        search.blur();
      }
    });
  }
  window.addEventListener('resize', () => {
    const dash = document.getElementById('view-dashboard');
    if (dash && dash.style.display !== 'none') renderDashboardServices();
  });
  document.addEventListener('keydown', e => {
    if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 'l') {
      e.preventDefault();
      const view = document.getElementById('view-servers');
      if (view && view.style.display === 'none') showServers();
      const el = document.getElementById('global-search');
      if (el) {
        el.focus();
        el.select();
      }
    }
  });
}

// ── Init ──────────────────────────────────────────────────────────────────────

async function init() {
  try {
    applySidebarCollapsed(localStorage.getItem('serverhub.sidebarCollapsed') === '1');
  } catch (_) {
    applySidebarCollapsed(false);
  }
  attachDelegatedHandlers();
  attachQADragHandlers();
  _loadDetailTab();

  // Sign out button is presence-gated: only for password-protected instances.
  try {
    updateSignoutVisibility(await fetch('/api/auth/status').then(r => r.json()));
  } catch (_) {}

  try {
    const cfg = await apiFetch('/api/appconfig');
    applyThemeConfig(cfg);
    applySizeUnits(cfg.size_units || 'GiB');
    applyAnimations(cfg.animations !== false);
    applyDashboardConfig(cfg.dashboard_order, cfg.dashboard_days !== false, cfg.dashboard_hidden);
    applyTerminalConfig(cfg.terminal);
    const bgBlur = cfg.background_image ? (cfg.background_blur ?? 12) : (cfg.background_blur || 12);
    applyBackgroundConfig(cfg.background_image || '', bgBlur);
    ownerName = (cfg.owner || '').trim();
  } catch (_) {
    applyThemeConfig({ hue: 120, shade: 20 });
    applySizeUnits('GiB');
    applyAnimations(true);
    applyDashboardConfig(['apps', 'favorites', 'quick_actions'], true, []);
    applyTerminalConfig();
    applyBackgroundConfig('', 12);
  }
  const greeting = document.getElementById('dashboard-greeting');
  if (greeting) greeting.textContent = ownerName ? `Hello, ${ownerName}` : 'Hello';
  updateDashboardClock();
  setInterval(updateDashboardClock, 1000);

  try {
    const info = await apiFetch('/api/local');
    localMachine = {
      ...localMachine,
      ...info,
      tags: info.tags || [],
      quick_actions: info.quick_actions || [],
    };
    localHostname = info.name || info.hostname || 'This Machine';
    noteServerClock(info.clock);
  } catch (_) {
    localHostname = 'This Machine';
    localMachine.name = localHostname;
  }

  await loadServers();
  routeFromLocation();
  // Start after the first render so the immediate snapshot lands on real cards.
  // Views rendered later reuse the same stream (it is never torn down).
  startDashboardStream();
  startScheduleAutoRefresh();
}

init();
