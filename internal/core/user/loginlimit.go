package user

import (
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Login throttling defaults. Reused as the constructor's only configuration
// today; if an operator ever needs to tune them, promote to a parameter
// then rather than before there is a real need to.
const (
	// maxLoginAttemptsPerUsername bounds guesses against one account,
	// regardless of which address they come from — the primary
	// credential-stuffing vector, and the one docs/adr/0013 asks to close.
	maxLoginAttemptsPerUsername = 5
	// maxLoginAttemptsPerIP bounds requests from one source regardless of
	// which username each one names — looser than the per-username budget,
	// because one address legitimately represents many real people behind a
	// NAT or corporate proxy, and locking out an office for one colleague's
	// typos is its own outage.
	maxLoginAttemptsPerIP = 20
	// loginWindow is the sliding window both budgets above are expressed
	// over.
	loginWindow = 15 * time.Minute
)

// LoginLimiter bounds POST /api/v1/auth/login attempts by two independent
// keys at once — username and source IP — so neither a distributed guess
// against one account nor a volumetric run against many accounts from one
// address escapes both budgets by only respecting the other.
//
// The per-username bucket resets on a successful login for that username
// (see [LoginLimiter.ResetUsername]), so a real user who mistyped their
// password a few times is not penalised on their next visit. The
// per-IP bucket deliberately never resets: if it did, an attacker holding
// one valid low-privilege credential could log in with it periodically
// purely to reset their own IP budget and keep guessing at other accounts
// from the same address — resetting only the account that just succeeded
// closes that gap.
type LoginLimiter struct {
	byUsername *keyedLimiters
	byIP       *keyedLimiters
}

// NewLoginLimiter builds a limiter with this package's fixed thresholds.
func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{
		byUsername: newKeyedLimiters(perWindow(maxLoginAttemptsPerUsername, loginWindow), maxLoginAttemptsPerUsername),
		byIP:       newKeyedLimiters(perWindow(maxLoginAttemptsPerIP, loginWindow), maxLoginAttemptsPerIP),
	}
}

// perWindow converts a count-per-window into the steady refill rate
// [rate.Limiter] wants, the same conversion [httpx.PerNodeRateLimit] uses
// for its own per-minute budget.
func perWindow(count int, window time.Duration) rate.Limit {
	return rate.Limit(float64(count) / window.Seconds())
}

// Allow reports whether a login attempt naming username, from sourceIP, may
// proceed. It must be called — and must consume a token either way — before
// any password verification happens: the budget exists to bound the cost of
// guessing, not just its eventual success.
//
// sourceIP may be empty (a caller with no meaningful address, e.g. a unit
// test); an empty key still gets its own bucket rather than being skipped,
// which is a harmless, deliberately simple choice — treating "unknown" as
// its own single bucket at worst pools together callers this limiter cannot
// distinguish anyway.
func (l *LoginLimiter) Allow(username, sourceIP string) bool {
	userOK := l.byUsername.allow(normalizeUsername(username))
	ipOK := l.byIP.allow(sourceIP)
	return userOK && ipOK
}

// ResetUsername clears username's failed-attempt budget after a successful
// login. It never touches the per-IP budget — see the type doc for why.
func (l *LoginLimiter) ResetUsername(username string) {
	l.byUsername.reset(normalizeUsername(username))
}

func normalizeUsername(username string) string {
	return strings.ToLower(strings.TrimSpace(username))
}

// keyedLimiters is [httpx.nodeLimiters]' shape generalised to any string
// key — a token bucket per key, idle buckets swept on use rather than by a
// background goroutine. Kept as its own small copy here rather than shared
// with httpx: this package must not depend on the HTTP layer, the same
// boundary [fleet] and [inventory] already keep from depending on Postgres.
type keyedLimiters struct {
	limit rate.Limit
	burst int

	mu      sync.Mutex
	byKey   map[string]*keyedLimiter
	lastGC  time.Time
	nowFunc func() time.Time
}

type keyedLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// limiterIdleTTL mirrors [httpx.limiterIdleTTL]'s reasoning: bound memory
// for keys that stop appearing, on a window long enough that a real login
// window never loses its bucket mid-use.
const limiterIdleTTL = 30 * time.Minute

func newKeyedLimiters(limit rate.Limit, burst int) *keyedLimiters {
	return &keyedLimiters{
		limit: limit, burst: burst,
		byKey: map[string]*keyedLimiter{}, nowFunc: time.Now,
	}
}

func (k *keyedLimiters) allow(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()

	now := k.nowFunc()
	l, ok := k.byKey[key]
	if !ok {
		l = &keyedLimiter{limiter: rate.NewLimiter(k.limit, k.burst)}
		k.byKey[key] = l
	}
	l.lastSeen = now

	if now.Sub(k.lastGC) > limiterIdleTTL {
		for id, entry := range k.byKey {
			if now.Sub(entry.lastSeen) > limiterIdleTTL {
				delete(k.byKey, id)
			}
		}
		k.lastGC = now
	}

	return l.limiter.AllowN(now, 1)
}

// reset drops key's bucket entirely, so its next Allow call starts fresh
// with a full burst rather than wherever the token count happened to be.
func (k *keyedLimiters) reset(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.byKey, key)
}
