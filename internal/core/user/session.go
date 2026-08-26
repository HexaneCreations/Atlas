package user

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// sessionTokenBytes matches [fleet.tokenBytes]: 256 bits, so brute-forcing a
// session cookie is not a meaningfully weaker attack than brute-forcing the
// TLS connection that carries it.
const sessionTokenBytes = 32

// GeneratedSession is a freshly created session, returned once.
//
// Plaintext is the raw cookie value, shown to nobody but the browser it is
// set on — there is no "reveal session token" API, the same rule
// [fleet.GeneratedToken] documents for enrollment tokens, and for the same
// reason: only [GeneratedSession.Hash] is ever persisted.
type GeneratedSession struct {
	Plaintext string
	Hash      string
}

// NewSession generates a fresh session token.
func NewSession() (GeneratedSession, error) {
	const op = "user.NewSession"

	buf := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return GeneratedSession{}, errs.Wrap(err, errs.CodeInternal, "could not generate session entropy").WithOp(op)
	}
	plaintext := hex.EncodeToString(buf)
	return GeneratedSession{Plaintext: plaintext, Hash: HashSessionToken(plaintext)}, nil
}

// HashSessionToken returns the stored form of a session token presented in a
// cookie.
//
// Plain SHA-256, not bcrypt: the input is 256 bits of generated entropy, not
// a human-chosen password, so there is nothing for a slow hash to protect
// against that the entropy does not already provide — the same reasoning
// [fleet.HashToken] documents for enrollment tokens. [HashPassword] uses
// bcrypt because a login password is the opposite case: low-entropy and
// human-chosen.
func HashSessionToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// Session is a server-side session record.
type Session struct {
	TokenHash string
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt *time.Time
}

// Live reports whether the session is still usable at now.
func (s Session) Live(now time.Time) bool {
	return s.RevokedAt == nil && now.Before(s.ExpiresAt)
}

// ErrSessionInvalid is returned when a session token cannot be resolved to a
// live session, for any reason — unknown, expired, or revoked. As with
// [ErrInvalidCredentials], the reason is deliberately not distinguished: a
// caller presenting a stale cookie must not be able to tell "expired" from
// "revoked by an operator" from "never existed".
var ErrSessionInvalid = errs.New(errs.CodeUnauthenticated, "the session is no longer valid")

// SessionStore is the storage surface for sessions. Implemented by
// internal/storage/user.Repository.
type SessionStore interface {
	// CreateSession persists a newly issued session.
	CreateSession(ctx context.Context, s Session) error
	// Resolve looks up the user a live session belongs to. Returns
	// [ErrSessionInvalid] for anything other than a live session — never a
	// zero Principal with a nil error, so a caller cannot mistake "invalid
	// session" for "an anonymous but valid request".
	Resolve(ctx context.Context, tokenHash string, now time.Time) (Principal, error)
	// RevokeSession invalidates a session, for logout.
	RevokeSession(ctx context.Context, tokenHash string, now time.Time) error
	// RevokeAllSessions invalidates every live session belonging to userID,
	// for an operator forcing a logout or disabling an account. actorUserID
	// is recorded in the audit trail, not used to authorize the call — the
	// caller has already checked PermissionUserManage.
	RevokeAllSessions(ctx context.Context, userID, actorUserID string, now time.Time) error
}
