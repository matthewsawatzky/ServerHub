// Package auth implements ServerHub's single-user password authentication:
//
//   - auth.json     bcrypt password hash (0600) in the config directory
//   - sessions.json active sessions, keyed by SHA-256 of the opaque token (0600)
//   - the auth middleware protecting every route except the public allowlist
//   - a global exponential login rate limiter
//
// Sessions survive restarts (persisted in sessions.json). Only the SHA-256 of
// each token is stored; the raw token lives solely in the session cookie.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"serverhub/internal/storage"
)

const (
	cookieName = "sh_session"

	sessionLifetime = 30 * 24 * time.Hour
	// Sliding extension: renew once at least a day has passed since the last extension.
	slidingWindow = 29 * 24 * time.Hour

	minPasswordLen = 8
	// bcrypt silently truncates beyond 72 bytes — reject instead.
	maxPasswordLen = 72

	bcryptCost        = 12
	authFormatVersion = 1
)

// Errors returned by Manager; the HTTP handlers map them to status codes.
var (
	ErrNoPassword      = errors.New("no password configured")
	ErrAlreadySetup    = errors.New("authentication is already configured")
	ErrInvalidPassword = errors.New("incorrect password")
	ErrRateLimited     = errors.New("too many login attempts")
	ErrWeakPassword    = errors.New("password does not meet requirements")
)

// Auth modes stored in auth.json:
//
//	ModeUnset    first run — onboarding wizard not completed yet
//	ModePassword password authentication (hash present)
//	ModeOptOut   owner explicitly declined a password; middleware passes
//	             everyone through (the Host/Origin/preflight guard still applies)
type Mode string

const (
	ModeUnset    Mode = ""
	ModePassword Mode = "password"
	ModeOptOut   Mode = "opt_out"
)

// LockedError indicates the login rate limiter is holding a lockout.
type LockedError struct {
	RetryAfter time.Duration
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many login attempts; retry after %s", e.RetryAfter)
}

// ── On-disk formats ───────────────────────────────────────────────────────────

type authFile struct {
	Version      int    `json:"version"`
	Mode         Mode   `json:"mode,omitempty"`
	PasswordHash string `json:"password_hash,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
}

type sessionEntry struct {
	TokenSHA256 string `json:"token_sha256"`
	CreatedAt   string `json:"created_at"`
	ExpiresAt   string `json:"expires_at"`
}

type sessionsFile struct {
	Version  int            `json:"version"`
	Sessions []sessionEntry `json:"sessions"`
}

// Manager owns the password hash and the active sessions.
// All state is guarded by mu; all file writes are atomic (temp file + rename).
type Manager struct {
	mu       sync.Mutex
	dir      string
	mode     Mode
	hash     []byte
	created  string
	sessions []sessionEntry

	limiter loginLimiter

	// Audit, when set, receives login failures and lockouts for the audit log.
	// The password value is never passed to it (or logged) anywhere.
	Audit func(serverID, action string)

	// Theme, when set, supplies the instance's current accent hue and shade
	// for the public status endpoint (the login page pre-applies the look —
	// cosmetics only, no secrets).
	Theme func() (hue, shade int)
}

// New loads (or initialises) auth state in dir. A missing auth.json means
// first run: no password exists yet. A corrupt/missing sessions.json is
// treated as "no active sessions" (users re-login) and logged loudly —
// never a permanent lockout.
func New(dir string) (*Manager, error) {
	if err := storage.EnsureDir(dir); err != nil {
		return nil, err
	}
	m := &Manager{dir: dir}
	if err := m.loadPassword(); err != nil {
		return nil, err
	}
	if err := m.loadSessions(); err != nil {
		return nil, err
	}
	return m, nil
}

const (
	authFileName     = "auth.json"
	sessionsFileName = "sessions.json"
)

func authPath(dir string) string { return filepath.Join(dir, authFileName) }
func sessPath(dir string) string { return filepath.Join(dir, sessionsFileName) }

func (m *Manager) loadPassword() error {
	data, err := os.ReadFile(authPath(m.dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil // first run — onboarding not completed yet
		}
		return fmt.Errorf("reading auth.json: %w", err)
	}
	var f authFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != authFormatVersion {
		fmt.Printf("warning: auth.json is corrupt or unreadable — treating as first run; delete auth.json and run ServerHub on the host for a clean state\n")
		return nil
	}
	if f.Mode == ModeOptOut {
		m.mode = ModeOptOut
		return nil
	}
	if f.PasswordHash == "" {
		// Unset mode without a hash: unusable — treat as first run.
		fmt.Printf("warning: auth.json has no password hash — treating as first run\n")
		return nil
	}
	m.mode = ModePassword
	m.hash = []byte(f.PasswordHash)
	m.created = f.CreatedAt
	return nil
}

func (m *Manager) loadSessions() error {
	data, err := os.ReadFile(sessPath(m.dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		fmt.Printf("warning: could not read sessions.json (%v) — sessions reset; users must log in again\n", err)
		return nil
	}
	var f sessionsFile
	if err := json.Unmarshal(data, &f); err != nil || f.Version != authFormatVersion {
		fmt.Printf("warning: sessions.json is corrupt (%v) — sessions reset; users must log in again\n", err)
		return nil
	}
	m.sessions = pruneSessions(f.Sessions, time.Now())
	return nil
}

// ── Password ──────────────────────────────────────────────────────────────────

// ChangePassword verifies the current password and replaces it. All sessions
// are revoked (the caller issues a fresh one for the current browser — see
// handleChange). Failed verifications feed the shared login rate limiter, so
// a stolen session cannot brute-force the current password.
func (m *Manager) ChangePassword(oldPassword, newPassword string) error {
	if err := validatePasswordValue(newPassword); err != nil {
		return err
	}
	m.mu.Lock()
	if m.mode != ModePassword || len(m.hash) == 0 {
		m.mu.Unlock()
		return ErrNoPassword
	}
	if retry, ok := m.limiter.try(); !ok {
		m.mu.Unlock()
		m.audit("password change blocked during lockout")
		return &LockedError{RetryAfter: retry}
	}
	hashCopy := make([]byte, len(m.hash))
	copy(hashCopy, m.hash)
	m.mu.Unlock()

	if bcrypt.CompareHashAndPassword(hashCopy, []byte(oldPassword)) != nil {
		m.mu.Lock()
		lock := m.limiter.fail()
		m.mu.Unlock()
		if lock > 0 {
			m.audit(fmt.Sprintf("password change failed — lockout engaged for %s", lock))
		} else {
			m.audit("password change failed — wrong current password")
		}
		return ErrInvalidPassword
	}

	h, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcryptCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	oldHash, oldCreated, oldSessions := m.hash, m.created, m.sessions
	newCreated := time.Now().UTC().Format(time.RFC3339)
	// Revoke every session first (persisted), then swap the hash.
	m.sessions = nil
	if err := m.writeSessions(); err != nil {
		m.sessions = oldSessions
		return err
	}
	m.hash = h
	m.created = newCreated
	if err := m.writeAuth(); err != nil {
		// Roll everything back to what the files say (the sessions.json on
		// disk still holds the old sessions — restore it best-effort).
		m.hash, m.created, m.sessions = oldHash, oldCreated, oldSessions
		if werr := m.writeSessions(); werr != nil {
			fmt.Printf("warning: could not restore sessions.json: %v\n", werr)
		}
		return err
	}
	m.audit("password changed — all sessions signed out")
	return nil
}

// HasPassword reports whether password authentication is active.
func (m *Manager) HasPassword() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode == ModePassword && len(m.hash) > 0
}

// AuthRequired reports whether requests need a valid session. Only
// ModePassword enforces authentication; first run and opt-out do not.
func (m *Manager) AuthRequired() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode == ModePassword
}

// OnboardingComplete reports whether the owner has gone through the first-run
// wizard in some form (password set, or password declined).
func (m *Manager) OnboardingComplete() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode != ModeUnset
}

// ModeActive returns the current auth mode.
func (m *Manager) ModeActive() Mode {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mode
}

// OptOut records the owner's decision to run without a password. This is
// written once; it can be reversed only via /api/auth/setup from loopback
// (sets a password) or by deleting auth.json and restarting.
func (m *Manager) OptOut() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModePassword {
		return ErrAlreadySetup
	}
	m.mode = ModeOptOut
	m.hash = nil
	m.created = time.Now().UTC().Format(time.RFC3339)
	if err := m.writeAuth(); err != nil {
		m.mode = ModeUnset
		return err
	}
	return nil
}

// SetupPassword configures password authentication (from first-run onboarding
// or, later, from a loopback client on an opted-out instance). Once a password
// exists it stays — changes go through recovery (delete auth.json, restart).
func (m *Manager) SetupPassword(password string) error {
	if err := validatePasswordValue(password); err != nil {
		return err
	}
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == ModePassword {
		return ErrAlreadySetup
	}
	prevMode, prevHash, prevCreated := m.mode, m.hash, m.created
	m.mode = ModePassword
	m.hash = h
	m.created = time.Now().UTC().Format(time.RFC3339)
	if err := m.writeAuth(); err != nil {
		m.mode, m.hash, m.created = prevMode, prevHash, prevCreated
		return err
	}
	return nil
}

func validatePasswordValue(pw string) error {
	if len(pw) < minPasswordLen {
		return fmt.Errorf("%w: minimum %d characters", ErrWeakPassword, minPasswordLen)
	}
	if len(pw) > maxPasswordLen {
		return fmt.Errorf("%w: maximum %d bytes (bcrypt truncates beyond this)", ErrWeakPassword, maxPasswordLen)
	}
	return nil
}

// ── Sessions ──────────────────────────────────────────────────────────────────

// CreateSession issues a new opaque token (used after login/setup success).
func (m *Manager) CreateSession() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.hash) == 0 {
		return "", ErrNoPassword
	}
	return m.createSessionLocked(), nil
}

func (m *Manager) createSessionLocked() string {
	token, err := newToken()
	if err != nil {
		// crypto/rand is unavailable: refusing to mint a guessable session token.
		panic("auth: crypto/rand unavailable: " + err.Error())
	}
	now := time.Now()
	entry := sessionEntry{
		TokenSHA256: sha256Hex(token),
		CreatedAt:   now.UTC().Format(time.RFC3339),
		ExpiresAt:   now.Add(sessionLifetime).UTC().Format(time.RFC3339),
	}
	m.sessions = append(m.sessions, entry)
	if err := m.writeSessions(); err != nil {
		fmt.Printf("warning: could not persist sessions.json: %v\n", err)
	}
	return token
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ValidateSession looks up token; extends it when the sliding window triggers
// (renewal cookie non-nil) and reports validity. Callers must treat `!ok` as
// an authentication failure.
func (m *Manager) ValidateSession(token string) (*http.Cookie, bool) {
	if token == "" {
		return nil, false
	}
	sum := sha256Hex(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	found := -1
	for i := range m.sessions {
		if subtle.ConstantTimeCompare([]byte(sum), []byte(m.sessions[i].TokenSHA256)) == 1 {
			found = i
			break
		}
	}
	if found == -1 {
		return nil, false
	}
	s := &m.sessions[found]
	exp, err := time.Parse(time.RFC3339, s.ExpiresAt)
	if err != nil || now.After(exp) {
		// Expired or unparsable expiry — drop it.
		m.sessions = append(m.sessions[:found], m.sessions[found+1:]...)
		if err := m.writeSessions(); err != nil {
			fmt.Printf("warning: could not persist sessions.json: %v\n", err)
		}
		return nil, false
	}
	if now.After(exp.Add(-slidingWindow)) {
		s.ExpiresAt = now.Add(sessionLifetime).UTC().Format(time.RFC3339)
		if err := m.writeSessions(); err != nil {
			fmt.Printf("warning: could not persist sessions.json: %v\n", err)
		}
		c := NewSessionCookie(token)
		return &c, true
	}
	return nil, true
}

// Valid performs a lookup without extending or persisting anything.
func (m *Manager) Valid(token string) bool {
	if token == "" {
		return false
	}
	sum := sha256Hex(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for i := range m.sessions {
		if subtle.ConstantTimeCompare([]byte(sum), []byte(m.sessions[i].TokenSHA256)) != 1 {
			continue
		}
		exp, err := time.Parse(time.RFC3339, m.sessions[i].ExpiresAt)
		return err == nil && now.Before(exp)
	}
	return false
}

// Logout revokes the given token (idempotent).
func (m *Manager) Logout(token string) {
	if token == "" {
		return
	}
	sum := sha256Hex(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.sessions {
		if subtle.ConstantTimeCompare([]byte(sum), []byte(m.sessions[i].TokenSHA256)) == 1 {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			break
		}
	}
	if err := m.writeSessions(); err != nil {
		fmt.Printf("warning: could not persist sessions.json: %v\n", err)
	}
}

// NewSessionCookie builds the session cookie. Secure is left to the caller
// (it depends on whether the request arrived over TLS).
func NewSessionCookie(value string) http.Cookie {
	return http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	}
}

func pruneSessions(in []sessionEntry, now time.Time) []sessionEntry {
	out := in[:0:0]
	for _, s := range in {
		exp, err := time.Parse(time.RFC3339, s.ExpiresAt)
		if err != nil || now.After(exp) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// ── Persistence ───────────────────────────────────────────────────────────────
//
// Files are written through the storage package: 0600, atomic temp+rename.

func (m *Manager) writeAuth() error {
	f := authFile{
		Version:      authFormatVersion,
		Mode:         m.mode,
		PasswordHash: string(m.hash),
		CreatedAt:    m.created,
	}
	return storage.WriteJSON(filepath.Join(m.dir, authFileName), f)
}

func (m *Manager) writeSessions() error {
	m.sessions = pruneSessions(m.sessions, time.Now())
	f := sessionsFile{
		Version:  authFormatVersion,
		Sessions: m.sessions,
	}
	return storage.WriteJSON(filepath.Join(m.dir, sessionsFileName), f)
}

// ── Global login rate limiter ─────────────────────────────────────────────────
//
// Single-user tool, so one global limiter (not per-IP): 5 failures → 30 s
// lockout; every further failure doubles the lockout, capped at 15 min.
// In-memory only — lockouts reset on restart (accepted trade-off).

type loginLimiter struct {
	fails       int
	lockedUntil time.Time
}

func (l *loginLimiter) try() (retryAfter time.Duration, ok bool) {
	if now := time.Now(); now.Before(l.lockedUntil) {
		return time.Until(l.lockedUntil).Round(time.Second), false
	}
	return 0, true
}

// fail records one failed attempt and engages a lockout from the 5th failure
// on. It returns the engaged lockout duration (0 while under the threshold).
func (l *loginLimiter) fail() time.Duration {
	l.fails++
	if l.fails < 5 {
		return 0
	}
	d := 30 * time.Second
	for i := 5; i < l.fails; i++ {
		d *= 2
	}
	if d > 15*time.Minute {
		d = 15 * time.Minute
	}
	l.lockedUntil = time.Now().Add(d)
	return d
}

func (l *loginLimiter) reset() {
	l.fails = 0
	l.lockedUntil = time.Time{}
}

// ── Login / logout core ───────────────────────────────────────────────────────

// Login verifies the password and returns a new session token. Lockouts are
// checked before verification, so even the correct password is refused while
// locked out. bcrypt runs outside the manager lock so session validation
// stays responsive while a lockout attempt is being hashed.
func (m *Manager) Login(password string) (string, error) {
	m.mu.Lock()
	if len(m.hash) == 0 {
		m.mu.Unlock()
		return "", ErrNoPassword
	}
	if retry, ok := m.limiter.try(); !ok {
		m.mu.Unlock()
		m.audit("login blocked during lockout")
		return "", &LockedError{RetryAfter: retry}
	}
	hashCopy := make([]byte, len(m.hash))
	copy(hashCopy, m.hash)
	m.mu.Unlock()

	if bcrypt.CompareHashAndPassword(hashCopy, []byte(password)) != nil {
		m.mu.Lock()
		lock := m.limiter.fail()
		m.mu.Unlock()
		if lock > 0 {
			m.audit(fmt.Sprintf("login failed — lockout engaged for %s", lock))
		} else {
			m.audit("login failed")
		}
		return "", ErrInvalidPassword
	}

	m.mu.Lock()
	m.limiter.reset()
	token := m.createSessionLocked()
	m.mu.Unlock()
	return token, nil
}

func (m *Manager) audit(action string) {
	if m.Audit != nil {
		m.Audit("auth", action)
	}
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

// RegisterRoutes registers the /api/auth/* endpoints on mux.
// /api/auth/change is NOT public — the middleware requires a session for it.
func RegisterRoutes(mux *http.ServeMux, m *Manager) {
	mux.HandleFunc("/api/auth/status", m.handleStatus)
	mux.HandleFunc("/api/auth/login", m.handleLogin)
	mux.HandleFunc("/api/auth/logout", m.handleLogout)
	mux.HandleFunc("/api/auth/setup", m.handleSetup)
	mux.HandleFunc("/api/auth/change", m.handleChange)
}

// ChangeBody is the JSON request body of /api/auth/change.
type ChangeBody struct {
	OldPassword string `json:"old_password"`
	NewPassword string `json:"new_password"`
}

func (m *Manager) handleChange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.HasPassword() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no password configured; use /api/auth/setup"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body ChangeBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := m.ChangePassword(body.OldPassword, body.NewPassword); err != nil {
		var locked *LockedError
		switch {
		case errors.As(err, &locked):
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(locked.RetryAfter.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]interface{}{
				"error":       ErrRateLimited.Error(),
				"retry_after": int(locked.RetryAfter.Seconds()) + 1,
			})
		case errors.Is(err, ErrInvalidPassword):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "current password is incorrect"})
		case errors.Is(err, ErrNoPassword):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no password configured"})
		default: // ErrWeakPassword
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}
	// All sessions (including the caller's) were revoked — issue a fresh one.
	token, err := m.CreateSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create session"})
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// LoginBody is the JSON request body of /api/auth/login and /api/auth/setup.
type LoginBody struct {
	Password string `json:"password"`
}

func decodeLoginBody(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body LoginBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return "", false
	}
	if body.Password == "" {
		http.Error(w, "password is required", http.StatusBadRequest)
		return "", false
	}
	return body.Password, true
}

func (m *Manager) handleStatus(w http.ResponseWriter, r *http.Request) {
	token, _ := tokenFromRequest(r)
	mode := m.ModeActive()
	// hue/shade are cosmetic values for the login page to pre-apply the
	// instance's look; public on purpose (no secrets in a colour).
	resp := struct {
		HasPassword        bool `json:"has_password"`
		PasswordOptOut     bool `json:"password_opt_out"`
		OnboardingComplete bool `json:"onboarding_complete"`
		AuthRequired       bool `json:"auth_required"`
		Authenticated      bool `json:"authenticated"`
		SetupAllowed       bool `json:"setup_allowed"`
		Hue                int  `json:"hue"`
		Shade              int  `json:"shade"`
	}{
		HasPassword:        m.HasPassword(),
		PasswordOptOut:     mode == ModeOptOut,
		OnboardingComplete: mode != ModeUnset,
		AuthRequired:       mode == ModePassword,
		Authenticated:      m.Valid(token) && mode == ModePassword,
		SetupAllowed:       mode != ModePassword && isLoopbackRemote(r),
		Hue:                m.themeHue(),
		Shade:              m.themeShade(),
	}
	writeJSON(w, http.StatusOK, resp)
}

func (m *Manager) themeHue() int {
	if m.Theme == nil {
		return 120
	}
	h, _ := m.Theme()
	return h
}

func (m *Manager) themeShade() int {
	if m.Theme == nil {
		return 20
	}
	_, s := m.Theme()
	return s
}

func (m *Manager) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	password, ok := decodeLoginBody(w, r)
	if !ok {
		return
	}
	token, err := m.Login(password)
	if err != nil {
		var locked *LockedError
		switch {
		case errors.As(err, &locked):
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(locked.RetryAfter.Seconds())+1))
			writeJSON(w, http.StatusTooManyRequests, map[string]interface{}{
				"error":       ErrRateLimited.Error(),
				"retry_after": int(locked.RetryAfter.Seconds()) + 1,
			})
		case errors.Is(err, ErrNoPassword):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "no password configured; use /api/auth/setup"})
		default: // ErrInvalidPassword
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": ErrInvalidPassword.Error()})
		}
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Manager) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token, _ := tokenFromRequest(r)
	m.Logout(token)
	clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (m *Manager) handleSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Setup configures authentication from the machine itself. It is refused
	// once password auth is active (mode opt-out or first-run may set one).
	if m.HasPassword() {
		http.Error(w, ErrAlreadySetup.Error(), http.StatusForbidden)
		return
	}
	if !isLoopbackRemote(r) {
		http.Error(w, "setup must be completed on the host running ServerHub", http.StatusForbidden)
		return
	}
	password, ok := decodeLoginBody(w, r)
	if !ok {
		return
	}
	if err := m.SetupPassword(password); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	m.audit("password configured (first-run setup)")
	token, err := m.CreateSession()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create session"})
		return
	}
	setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ── Cookie helpers ────────────────────────────────────────────────────────────

func tokenFromRequest(r *http.Request) (string, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	return c.Value, true
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	c := NewSessionCookie(token)
	c.Secure = r.TLS != nil // false on loopback HTTP; true automatically behind TLS
	http.SetCookie(w, &c)
}

func clearSessionCookie(w http.ResponseWriter) {
	c := http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode}
	http.SetCookie(w, &c)
}

// ── Shared helpers ────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// isLoopbackRemote reports whether the request came from this machine
// (127.0.0.1 or ::1 or any loopback interface).
func isLoopbackRemote(r *http.Request) bool {
	return net.ParseIP(remoteIP(r)).IsLoopback()
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// ── Route middleware ──────────────────────────────────────────────────────────

// publicPaths are the only routes reachable without a valid session.
// Everything else (including every WebSocket upgrade) requires one while
// password auth is active. /api/onboarding and /onboarding-image enforce
// their own tighter rules inside their handlers.
var publicRoutes = map[string]struct{}{
	"/login":            {},
	"/api/auth/status":  {},
	"/api/auth/login":   {},
	"/api/auth/setup":   {},
	"/api/onboarding":   {},
	"/onboarding-image": {},
	"/api/ping":         {},
}

// Middleware enforces access:
//
//   - ModeOptOut: every request passes (the Host/Origin/preflight guard still applies).
//     This is the only pass-through mode — it is an explicit owner decision.
//   - ModeUnset (first run): requires "a session"; none can exist yet, so every
//     HTML request lands on the onboarding wizard at /login and every other
//     request gets 401. Nothing is usable until setup completes.
//   - ModePassword: requires a valid session cookie.
//
// It must wrap the whole mux — a WebSocket handshake is an ordinary HTTP
// request, so upgrades are covered before Upgrade() runs.
func Middleware(m *Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.ModeActive() == ModeOptOut {
			next.ServeHTTP(w, r)
			return
		}
		if _, pub := publicRoutes[r.URL.Path]; pub {
			next.ServeHTTP(w, r)
			return
		}
		token, _ := tokenFromRequest(r)
		renewed, ok := m.ValidateSession(token)
		if ok {
			if renewed != nil {
				renewed.Secure = r.TLS != nil
				http.SetCookie(w, renewed)
			}
			next.ServeHTTP(w, r)
			return
		}
		if wantsHTML(r) {
			next_ := r.URL.Path
			if r.URL.RawQuery != "" {
				next_ += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next_), http.StatusFound)
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
	})
}

// AuthenticateRequest validates the session cookie of a request. Used by
// routes that do their own gating (e.g. the onboarding image endpoint).
func (m *Manager) AuthenticateRequest(r *http.Request) bool {
	token, _ := tokenFromRequest(r)
	return m.Valid(token)
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
