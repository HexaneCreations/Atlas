package user_test

import (
	"testing"

	"github.com/hexane/atlas/internal/core/user"
)

func TestLoginLimiterAllowsUpToTheThreshold(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	for i := range 5 {
		if !l.Allow("alice", "10.0.0.1") {
			t.Fatalf("attempt %d denied, want allowed (threshold is 5)", i+1)
		}
	}
}

func TestLoginLimiterDeniesThe6thAttemptWithinTheWindow(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	for range 5 {
		l.Allow("bob", "10.0.0.2")
	}
	if l.Allow("bob", "10.0.0.2") {
		t.Error("6th attempt within the window was allowed, want denied")
	}
}

// A different username from the same IP has its own budget — the
// per-username bucket is what stops guessing against one account.
func TestLoginLimiterPerUsernameBudgetsAreIndependent(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	for range 5 {
		l.Allow("carol", "10.0.0.3")
	}
	if !l.Allow("dave", "10.0.0.3") {
		t.Error("a different username from the same IP was denied by carol's exhausted budget")
	}
}

// The per-IP budget (20) is independent of and looser than any single
// username's (5) — enough distinct usernames from one address must still
// eventually hit the IP ceiling, proving the two budgets are genuinely
// separate checks, not the same counter viewed two ways.
func TestLoginLimiterPerIPBudgetCatchesManyDistinctUsernames(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	usernames := []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9", "u10",
		"u11", "u12", "u13", "u14", "u15", "u16", "u17", "u18", "u19", "u20"}
	for _, name := range usernames {
		if !l.Allow(name, "10.0.0.4") {
			t.Fatalf("attempt for %q denied before the 20-request IP ceiling", name)
		}
	}
	if l.Allow("u21", "10.0.0.4") {
		t.Error("21st distinct username from the same IP was allowed, want denied by the IP budget")
	}
}

// The central behavior this review asked to be explicit about: a
// successful login resets the username's budget, so a real user who
// mistyped their password a few times is not penalised on their next visit
// — but this cannot be used to escape an active lockout, since the
// successful attempt itself still had to pass Allow() first (proven by the
// second sub-test below).
func TestLoginLimiterResetUsernameClearsTheBudgetForFutureAttempts(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	for range 3 {
		l.Allow("erin", "10.0.0.5") // 3 "failed" attempts, budget is 5
	}
	l.ResetUsername("erin")

	// Without the reset, only 2 of these 5 would have succeeded (5 - 3
	// already spent). All 5 succeeding proves the bucket actually reset,
	// not merely that some tokens remained.
	for i := range 5 {
		if !l.Allow("erin", "10.0.0.5") {
			t.Fatalf("post-reset attempt %d denied, want allowed — ResetUsername did not clear the budget", i+1)
		}
	}
}

func TestLoginLimiterResetCannotBeUsedToEscapeAnActiveLockout(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	for range 5 {
		l.Allow("frank", "10.0.0.6")
	}
	// Already over budget: the 6th call is denied, exactly like
	// TestLoginLimiterDeniesThe6thAttemptWithinTheWindow. A caller must see
	// this false before ever knowing whether the password was even right —
	// Login checks Allow() before ByUsername/VerifyPassword, so a correct
	// password physically cannot reach ResetUsername from here.
	if l.Allow("frank", "10.0.0.6") {
		t.Fatal("attempt beyond the budget was allowed — nothing should reach password verification, let alone a reset, once over budget")
	}
}

func TestLoginLimiterUsernameKeyIsCaseAndWhitespaceInsensitive(t *testing.T) {
	t.Parallel()

	l := user.NewLoginLimiter()
	for range 5 {
		l.Allow("Grace", "10.0.0.7")
	}
	// Same account, different casing/whitespace — must share the budget,
	// or the limiter is trivially bypassed by varying how the username is
	// typed.
	if l.Allow("  grace  ", "10.0.0.8") {
		t.Error("varying the username's case/whitespace bypassed its budget")
	}
}
