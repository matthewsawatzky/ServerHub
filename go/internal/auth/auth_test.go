package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestSetupLoginValidateLogout(t *testing.T) {
	m := newTestManager(t)
	if m.AuthRequired() || m.OnboardingComplete() {
		t.Fatal("fresh manager should be unset")
	}

	if err := m.SetupPassword("correct horse battery"); err != nil {
		t.Fatalf("SetupPassword: %v", err)
	}
	if !m.AuthRequired() || !m.OnboardingComplete() {
		t.Fatal("password mode should require auth and be onboarded")
	}

	if _, err := m.Login("wrong password"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("wrong password error = %v, want ErrInvalidPassword", err)
	}

	token, err := m.Login("correct horse battery")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if token == "" || !m.Valid(token) {
		t.Fatal("issued session token should be valid")
	}
	if renewed, ok := m.ValidateSession(token); !ok || renewed != nil {
		t.Fatalf("ValidateSession = ok:%v renewed:%v, want ok and no renewal", ok, renewed)
	}

	m.Logout(token)
	if m.Valid(token) {
		t.Fatal("token should be invalid after logout")
	}
}

func TestPasswordPolicyAndDuplicateSetup(t *testing.T) {
	m := newTestManager(t)

	if err := m.SetupPassword("short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short password error = %v, want ErrWeakPassword", err)
	}
	long := strings.Repeat("a", maxPasswordLen+1)
	if err := m.SetupPassword(long); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("overlong password error = %v, want ErrWeakPassword", err)
	}

	if err := m.SetupPassword("long enough password"); err != nil {
		t.Fatalf("SetupPassword: %v", err)
	}
	if err := m.SetupPassword("another long password"); !errors.Is(err, ErrAlreadySetup) {
		t.Fatalf("second setup error = %v, want ErrAlreadySetup", err)
	}
}

func TestOptOutThenSetPassword(t *testing.T) {
	m := newTestManager(t)
	if err := m.OptOut(); err != nil {
		t.Fatalf("OptOut: %v", err)
	}
	if m.AuthRequired() {
		t.Fatal("opt-out mode must not require auth")
	}
	if !m.OnboardingComplete() {
		t.Fatal("opt-out counts as completed onboarding")
	}
	if err := m.OptOut(); err != nil {
		t.Fatalf("OptOut should be idempotent in opt-out mode: %v", err)
	}
	if err := m.SetupPassword("password from opt out"); err != nil {
		t.Fatalf("SetupPassword after opt-out: %v", err)
	}
	if !m.AuthRequired() {
		t.Fatal("password mode should require auth")
	}
}

func TestChangePasswordRevokesAllSessions(t *testing.T) {
	m := newTestManager(t)
	if err := m.SetupPassword("current password"); err != nil {
		t.Fatalf("SetupPassword: %v", err)
	}
	oldToken, err := m.Login("current password")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	if err := m.ChangePassword("wrong current", "new password"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("wrong current password error = %v, want ErrInvalidPassword", err)
	}
	if err := m.ChangePassword("current password", "new password"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	m.mu.Lock()
	fails := m.limiter.fails
	m.mu.Unlock()
	if fails != 0 {
		t.Fatalf("limiter fails after successful password change = %d, want 0", fails)
	}
	if m.Valid(oldToken) {
		t.Fatal("old session must be revoked after a password change")
	}
	if _, err := m.Login("current password"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("old password still works: %v", err)
	}
	if _, err := m.Login("new password"); err != nil {
		t.Fatalf("new password login: %v", err)
	}
}

func TestExpiredSessionIsRejectedAndPruned(t *testing.T) {
	m := newTestManager(t)
	if err := m.SetupPassword("password for expiry"); err != nil {
		t.Fatalf("SetupPassword: %v", err)
	}
	token, err := m.Login("password for expiry")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}

	m.mu.Lock()
	m.sessions[0].ExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	m.mu.Unlock()

	if _, ok := m.ValidateSession(token); ok {
		t.Fatal("expired session must not validate")
	}
	m.mu.Lock()
	n := len(m.sessions)
	m.mu.Unlock()
	if n != 0 {
		t.Fatalf("expired session should be pruned, %d remain", n)
	}
}

func TestLoginRateLimiterLocksOutEvenCorrectPassword(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("the right password"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	m := &Manager{dir: t.TempDir(), mode: ModePassword, hash: hash}

	for i := 0; i < 5; i++ {
		if _, err := m.Login("wrong"); !errors.Is(err, ErrInvalidPassword) {
			t.Fatalf("attempt %d error = %v, want ErrInvalidPassword", i+1, err)
		}
	}

	_, err = m.Login("the right password")
	var locked *LockedError
	if !errors.As(err, &locked) {
		t.Fatalf("correct password during lockout error = %v, want LockedError", err)
	}
	if locked.RetryAfter <= 0 {
		t.Fatal("lockout should report a positive retry-after")
	}
}

func TestNoPasswordConfigured(t *testing.T) {
	m := newTestManager(t)
	if _, err := m.Login("anything"); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("Login on unset manager error = %v, want ErrNoPassword", err)
	}
	if _, err := m.CreateSession(); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("CreateSession on unset manager error = %v, want ErrNoPassword", err)
	}
}
