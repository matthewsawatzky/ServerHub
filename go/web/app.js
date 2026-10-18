'use strict';

// ── Full theme definitions ────────────────────────────────────────────────────

// ── Color config (hue + shade sliders) ───────────────────────────────────────

let accentHue = 120;
let shadeLightness = 20;
let animationsEnabled = true;

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

function applyColorConfig(hue, shade) {
  accentHue = hue;
  shadeLightness = shade = Math.min(100, Math.max(0, shade));

  const accentL = accentLightness(hue, shade);
  const set = (name, val) => document.documentElement.style.setProperty(name, val);
  set('--accent',      `hsl(${hue},70%,${accentL}%)`);
  set('--accent-dim',  `hsla(${hue},70%,${accentL}%,0.15)`);
  set('--accent-text', accentTextColor(hue, accentL));
  set('--acceptance-green', '#4caf50');
  set('--green',      '#4caf50');
  set('--amber',      '#ffb300');
  set('--red',        '#e53935');
  set('--overlay',    'rgba(0,0,0,0.60)');
  set('--shadow',     'rgba(0,0,0,0.45)');
  set('--font-ui',    "system-ui,-apple-system,'Segoe UI',sans-serif");
  set('--font-mono',  "'Cascadia Code','Fira Code','Consolas','Menlo',monospace");
  shadeSurfaceVars(shade, set);

  const hueEl = document.getElementById('hue-slider');
  if (hueEl) hueEl.value = hue;
  const shadeEl = document.getElementById('shade-slider');
  if (shadeEl) shadeEl.value = shade;
}

function applyAnimations(enabled) {
  animationsEnabled = enabled;
  document.documentElement.classList.toggle('no-anim', !enabled);
  const cb = document.getElementById('anim-toggle');
  if (cb) cb.checked = enabled;
}

let _saveTimer = null;
async function _saveAppConfig() {
  clearTimeout(_saveTimer);
  _saveTimer = setTimeout(async () => {
    try {
      await fetch('/api/appconfig', {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ hue: accentHue, shade: shadeLightness, animations: animationsEnabled }),
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
  const bg = Math.max(2, Math.min(100, shadeLightness));
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
let localMachine = {
  id: '__local__',
  name: '',
  hostname: '',
  online: true,
  tags: [],
  notes: '',
  quick_actions: [],
};
let dashboardES = null;
let detailES = null;
let localDetailES = null;

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

// ── Navigation ────────────────────────────────────────────────────────────────

function showView(id) {
  ['view-dashboard', 'view-detail', 'view-settings'].forEach(v => {
    document.getElementById(v).style.display = v === id ? '' : 'none';
  });
  document.getElementById('nav-dashboard').classList.toggle('active', id === 'view-dashboard');
  document.getElementById('nav-settings').classList.toggle('active', id === 'view-settings');
  clearAllPolls();
  stopAllQARuns();
}

function showDashboard() {
  showView('view-dashboard');
  currentServerId = null;
  updateSidebarActive(null);
  startDashboardStream();
}

function showSettings() {
  showView('view-settings');
  currentServerId = null;
  updateSidebarActive(null);
  showSettingsTab('general');
}

// ── Settings tabs ────────────────────────────────────────────────────────────

function showSettingsTab(name) {
  for (const tab of ['general', 'security', 'terminal']) {
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

function showDetail(id) {
  currentServerId = id;
  showView('view-detail');
  updateSidebarActive(id);
  renderDetail(id);
  if (id === '__local__') startLocalDetailStream();
  else startDetailStream(id);
}

function updateSidebarActive(id) {
  const navLocal = document.getElementById('nav-local');
  if (navLocal) navLocal.classList.toggle('active', id === '__local__');
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
      const body = await r.json();
      if (body && body.error) message = body.error;
    } catch (_) {}
    const err = new Error(message);
    err.status = r.status;
    throw err;
  }
  return r.json();
}

// ── Sidebar ───────────────────────────────────────────────────────────────────

function renderSidebar() {
  const online = servers.filter(s => s.online).length;
  document.getElementById('brand-sub').textContent =
    `${servers.length} server${servers.length !== 1 ? 's' : ''} · ${online} online`;

  const list = document.getElementById('server-list');
  list.innerHTML = servers.map(s => `
    <div class="server-nav-item${s.id === currentServerId ? ' active' : ''}"
         data-id="${esc(s.id)}">
      <div class="status-dot ${s.online ? 'dot-online' : 'dot-offline'}"></div>
      <span>${esc(s.name)}</span>
    </div>
  `).join('');
}

// ── Dashboard ─────────────────────────────────────────────────────────────────

async function loadServers() {
  try {
    servers = await apiFetch('/api/servers');
    if (!servers) servers = [];
    renderSidebar();
    renderDashboardCards();
  } catch (e) {
    renderDashboardCards();
    document.getElementById('server-cards').innerHTML =
      `<div class="loading-msg" style="grid-column:1/-1">Failed to load servers: ${esc(e.message)}</div>`;
  }
}

// ── Dashboard SSE stream ──────────────────────────────────────────────────────

function startDashboardStream() {
  if (dashboardES) { dashboardES.close(); dashboardES = null; }
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
  if (snap.servers) {
    snap.servers.forEach(card => {
      const s = servers.find(x => x.id === card.id);
      if (s) {
        s.online = card.online;
        updateDashboardCard(card.id, card, card.online);
      }
    });
    renderSidebar();
  }
  if (snap.local) {
    if (snap.local.name && snap.local.name !== localHostname) {
      localHostname = snap.local.name;
      localMachine.name = snap.local.name;
      const cardName = document.querySelector('#card-__local__ .card-name');
      if (cardName) cardName.textContent = localHostname;
      renderSidebar();
    }
    updateDashboardCard('__local__', snap.local, true);
  }
}

function localCardHTML() {
  const tags = localMachine.tags && localMachine.tags.length
    ? `<div class="tags">${localMachine.tags.map(t => `<span class="tag">${esc(t)}</span>`).join('')}</div>`
    : '';
  return `
  <div class="server-card local-card" id="card-__local__" data-id="__local__">
    <div class="card-header">
      <span class="card-name">${esc(localHostname)}</span>
      <span class="badge badge-local" id="card-badge-__local__">local</span>
    </div>
    <div class="stat-bars" id="card-bars-__local__">
      <div style="color:var(--text-dim);font-size:11px;">Loading stats…</div>
    </div>
    <div class="card-footer" id="card-footer-__local__">
      <span>—</span><span>—</span>
    </div>
    ${tags}
  </div>`;
}

function renderDashboardCards() {
  const localWrap = document.getElementById('local-cards');
  const localSection = document.getElementById('local-section');
  if (localWrap && localSection) {
    if (localHostname) {
      localSection.style.display = '';
      localWrap.innerHTML = localCardHTML();
    } else {
      localSection.style.display = 'none';
    }
  }

  const container = document.getElementById('server-cards');
  if (!servers.length) {
    container.innerHTML = `
      <div style="grid-column:1/-1;text-align:center;padding:48px 0">
        <div style="color:var(--text-dim);font-size:13px;margin-bottom:16px">No servers yet. Add one to get started.</div>
        <button class="btn btn-primary" onclick="openAddServer()">
          ${icon('plus', 14)} Add Server
        </button>
      </div>`;
    return;
  }
  container.innerHTML = servers.map(s => serverCardHTML(s)).join('');
}

function serverCardHTML(s) {
  const cls = s.online ? '' : ' offline';
  return `
  <div class="server-card${cls}" id="card-${esc(s.id)}" data-id="${esc(s.id)}">
    <div class="card-header">
      <span class="card-name">${esc(s.name)}</span>
      <span class="badge ${s.online ? 'badge-online' : 'badge-offline'}" id="card-badge-${esc(s.id)}">
        ${s.online ? 'online' : 'offline'}
      </span>
    </div>
    <div class="stat-bars" id="card-bars-${esc(s.id)}">
      ${s.online ? '<div style="color:var(--text-dim);font-size:11px;">Loading stats…</div>' : ''}
    </div>
    <div class="card-footer" id="card-footer-${esc(s.id)}">
      ${s.online ? '<span>—</span><span>—</span>' : '<span>Offline</span><span></span>'}
    </div>
    ${s.tags && s.tags.length ? `<div class="tags">${s.tags.map(t => `<span class="tag">${esc(t)}</span>`).join('')}</div>` : ''}
  </div>`;
}

function updateDashboardCard(id, stats, online) {
  const bars   = document.getElementById(`card-bars-${id}`);
  const badge  = document.getElementById(`card-badge-${id}`);
  const footer = document.getElementById(`card-footer-${id}`);
  const card   = document.getElementById(`card-${id}`);
  if (!bars) return;

  const isLocal = id === '__local__';

  if (!isLocal && (!online || !stats.online)) {
    card && card.classList.add('offline');
    if (badge)  { badge.textContent = 'offline'; badge.className = 'badge badge-offline'; }
    if (bars)   bars.innerHTML = '';
    if (footer) footer.innerHTML = '<span>Offline</span><span></span>';
    return;
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
  fetchAndRenderContainers('__local__');
  clearInterval(pollTimers['__local__containers']);
  pollTimers['__local__containers'] = setInterval(
    () => fetchAndRenderContainers('__local__'), 5000
  );
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
  document.getElementById('stat-grid').innerHTML =
    '<div class="loading-msg">Loading stats…</div>';
  document.getElementById('containers-body').innerHTML =
    '<tr><td colspan="4" style="color:var(--text-dim);font-size:12px;padding:16px 10px;">Loading…</td></tr>';
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

  const memUsedGB  = (stats.mem_used  / 1024 / 1024).toFixed(1);
  const memTotalGB = (stats.mem_total / 1024 / 1024).toFixed(1);
  const diskUsedGB  = (stats.disk_used  / 1024 / 1024).toFixed(1);
  const diskTotalGB = (stats.disk_total / 1024 / 1024).toFixed(1);

  const cards = [
    { label: 'CPU Usage',    value: `${stats.cpu ?? 0}%`, sub: `load ${stats.load1||'—'} / ${stats.load5||'—'} / ${stats.load15||'—'}`, hist: (stats.history||[]).map(p=>p.cpu) },
    { label: 'Memory',       value: `${stats.mem_pct ?? 0}%`, sub: `${memUsedGB} / ${memTotalGB} GB`, hist: (stats.history||[]).map(p=>p.mem_pct) },
    { label: 'Disk (/)',     value: `${stats.disk_pct ?? 0}%`, sub: `${diskUsedGB} / ${diskTotalGB} GB`, hist: (stats.history||[]).map(p=>p.disk_pct) },
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
    setTimeout(() => fetchAndRenderContainers(serverId), 1200);
  } catch (e) {
    toast(`Failed: ${e.message}`, 'err');
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
    return `
      <div class="qa-card" id="qa-item-${esc(s.id)}-${i}" data-qa-idx="${i}">
        <div class="qa-card-top">
          <div class="qa-card-heading">
            <span class="qa-card-name">${esc(qa.label)}</span>
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
          <button type="button" class="qa-play-btn" data-server="${esc(s.id)}" data-qa-idx="${i}" data-qa-run>run</button>
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
  if (editing && qaModalIndex >= 0 && qaModalIndex < actions.length) {
    actions[qaModalIndex] = { ...actions[qaModalIndex], label, command };
  } else {
    actions.push({ label, command });
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

function setQAOutput(text, empty) {
  const out = document.getElementById('qa-output');
  if (!out) return;
  out.textContent = text;
  out.classList.toggle('empty', !!empty);
}

function resetQAOutput() {
  const field = document.getElementById('qa-output-field');
  if (field) field.classList.remove('running');
  setQAOutput('No output yet.', true);
}

function qaRowId(serverId, idx) {
  return `qa-item-${serverId}-${idx}`;
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
  const field = document.getElementById('qa-output-field');
  if (field) field.classList.remove('running');
}

function toggleQA(serverId, idx) {
  const rowId = qaRowId(serverId, idx);
  if (qaRuns.has(rowId)) stopQA(rowId);
  else runQA(serverId, idx);
}

async function runQA(serverId, idx) {
  const rowId = qaRowId(serverId, idx);
  const row = document.getElementById(rowId);
  const field = document.getElementById('qa-output-field');
  if (!row || !field || qaRuns.has(rowId)) return;

  const statusEl = row.querySelector('.qa-action-status');
  const lastEl = row.querySelector('.qa-card-last');
  const durationEl = row.querySelector('.qa-action-duration');
  const btn = row.querySelector('[data-qa-run]');
  const actionLabel = row.querySelector('.qa-card-name')
    ? row.querySelector('.qa-card-name').textContent
    : '';
  const controller = new AbortController();
  const started = performance.now();
  const run = { controller, timer: null };
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

  setQAOutput('Running…', true);
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
    status = r.headers.get('X-Quick-Action-Status') === 'error' ? 'error' : 'success';
    if (currentServerId === serverId) {
      setQAOutput(text || '(no output)', false);
      toast(status === 'success' ? 'Action completed' : 'Action failed',
            status === 'success' ? 'ok' : 'err');
    }
  } catch (e) {
    if (e.name === 'AbortError') {
      status = 'stopped';
      if (currentServerId === serverId) {
        setQAOutput('Stopped.', false);
        toast('Action stopped', 'info');
      }
    } else {
      status = 'error';
      if (currentServerId === serverId) {
        setQAOutput('Error: ' + e.message, false);
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
          status === 'stopped' ? 'status-stopped' : 'status-error'
        );
        statusEl.textContent = status === 'success' ? 'success' : status === 'stopped' ? 'stopped' : 'failed';
      }
      if (lastEl) lastEl.textContent = _formatLastRan(now);
      if (btn) {
        btn.textContent = 'run';
        btn.classList.remove('stop');
      }
    }
    if (currentServerId === serverId && qaRuns.size === 0) {
      field.classList.remove('running');
    }
  }
}

function startDetailStream(id) {
  stopDetailStream();
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
  stopDashboardStream();
  stopDetailStream();
  stopLocalDetailStream();
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
  (s.quick_actions || []).forEach(qa => addQARow(qa.label, qa.command));
}

function addQARow(label = '', command = '') {
  const id = qaRowCount++;
  const row = document.createElement('div');
  row.className = 'qa-form-row';
  row.id = `qa-row-${id}`;
  row.innerHTML = `
    <input class="form-input qa-form-label" type="text" placeholder="Label" value="${esc(label)}" data-qa-field="label">
    <input class="form-input qa-form-cmd" type="text" placeholder="Command" value="${esc(command)}" data-qa-field="command" style="font-family:var(--font-mono);font-size:12px">
    <button type="button" class="qa-remove-btn" onclick="removeQARow('qa-row-${id}')" title="Remove">
      ${icon('x', 12)}
    </button>`;
  document.getElementById('qa-form-list').appendChild(row);
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
  const quick_actions = Array.from(qaRows).map(row => ({
    label:   row.querySelector('[data-qa-field="label"]').value.trim(),
    command: row.querySelector('[data-qa-field="command"]').value.trim(),
  })).filter(qa => qa.label && qa.command);
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
  document.getElementById('delete-modal').style.display = 'flex';
  document.addEventListener('keydown', deleteKeyHandler);
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
  try {
    await apiFetch(`/api/server/${id}`, { method: 'DELETE' });
    servers = servers.filter(x => x.id !== id);
    closeDeleteModal();
    renderSidebar();
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

  on('server-list', '.server-nav-item', el => showDetail(el.dataset.id));
  on('server-cards', '.server-card', el => showDetail(el.dataset.id));
  on('local-cards', '.server-card', el => showDetail(el.dataset.id));

  on('containers-body', '[data-container-action]', el =>
    containerAction(el.dataset.server, el.dataset.name, el.dataset.containerAction));

  on('qa-list', '[data-qa-run]', el =>
    toggleQA(el.dataset.server, parseInt(el.dataset.qaIdx, 10) || 0));
  on('qa-list', '[data-qa-edit]', el =>
    openQAModal(parseInt(el.dataset.qaEdit, 10)));

  on('action-row', '[data-confirm-yes]', el => runServerAction(el.dataset.id, el.dataset.action));
  on('action-row', '[data-confirm-cancel]', el => cancelConfirm(el.dataset.id, el.dataset.action));
  on('action-row', '[data-act]', el => confirmAction(el.dataset.id, el.dataset.act));
}

// ── Init ──────────────────────────────────────────────────────────────────────

async function init() {
  attachDelegatedHandlers();
  attachQADragHandlers();

  // Sign out button is presence-gated: only for password-protected instances.
  try {
    updateSignoutVisibility(await fetch('/api/auth/status').then(r => r.json()));
  } catch (_) {}

  try {
    const cfg = await apiFetch('/api/appconfig');
    applyColorConfig(cfg.hue ?? 120, cfg.shade ?? 20);
    applyAnimations(cfg.animations !== false);
    applyTerminalConfig(cfg.terminal);
    ownerName = (cfg.owner || '').trim();
  } catch (_) {
    applyColorConfig(120, 20);
    applyAnimations(true);
    applyTerminalConfig();
  }
  const greeting = document.getElementById('dashboard-greeting');
  if (greeting && ownerName) {
    greeting.textContent = `Welcome back, ${ownerName}`;
    greeting.style.display = '';
  }

  try {
    const info = await apiFetch('/api/local');
    localMachine = {
      ...localMachine,
      ...info,
      tags: info.tags || [],
      quick_actions: info.quick_actions || [],
    };
    localHostname = info.name || info.hostname || 'This Machine';
  } catch (_) {
    localHostname = 'This Machine';
    localMachine.name = localHostname;
  }

  await loadServers();
  startDashboardStream();
}

init();
