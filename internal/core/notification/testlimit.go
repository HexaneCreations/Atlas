package notification

import (
	"sync"
	"time"
)

// Test-send limits. Five seconds is longer than a normal send and shorter
// than anyone fixing a form; ten a minute is far above what debugging a
// channel takes, and far below what would make Atlas useful for flooding a
// destination or probing hosts.
const (
	TestMinGap     = 5 * time.Second
	TestUserWindow = time.Minute
	TestUserMax    = 10
)

// TestLimiter bounds test sends per user and per target (a channel, or a
// draft's host). Only an admitted test is recorded, and records older than
// TestUserWindow are dropped on every call, so the limiter never holds more
// than TestUserMax targets per active user.
type TestLimiter struct {
	now func() time.Time

	mu       sync.Mutex
	byUser   map[string][]time.Time
	byTarget map[string]time.Time
}

// NewTestLimiter builds a limiter reading the time from now.
func NewTestLimiter(now func() time.Time) *TestLimiter {
	return &TestLimiter{now: now, byUser: map[string][]time.Time{}, byTarget: map[string]time.Time{}}
}

// Allow reports whether userID may send a test to target now, and if not,
// how long until it may.
func (l *TestLimiter) Allow(userID, target string) (retryAfter time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.evict(now)

	wait := func(until time.Time) { retryAfter = max(retryAfter, until.Sub(now)) }
	sent := l.byUser[userID]
	if len(sent) > 0 {
		wait(sent[len(sent)-1].Add(TestMinGap))
	}
	if len(sent) >= TestUserMax {
		wait(sent[len(sent)-TestUserMax].Add(TestUserWindow))
	}
	if last, found := l.byTarget[target]; found {
		wait(last.Add(TestMinGap))
	}
	if retryAfter > 0 {
		return retryAfter, false
	}

	l.byUser[userID] = append(sent, now)
	l.byTarget[target] = now
	return 0, true
}

func (l *TestLimiter) evict(now time.Time) {
	for user, sent := range l.byUser {
		i := 0
		for i < len(sent) && now.Sub(sent[i]) >= TestUserWindow {
			i++
		}
		if i == len(sent) {
			delete(l.byUser, user)
		} else {
			l.byUser[user] = sent[i:]
		}
	}
	for target, last := range l.byTarget {
		if now.Sub(last) >= TestMinGap {
			delete(l.byTarget, target)
		}
	}
}
