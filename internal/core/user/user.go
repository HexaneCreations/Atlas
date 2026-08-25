// Package user is the domain logic of human-user identity, authentication,
// and role-based authorization for the Atlas control plane.
//
// This is deliberately a separate identity domain from [fleet]: a human user
// is never a libp2p Peer ID and never appears in agent_peers or
// agent_operation_grants, and an Atlas Agent never authenticates as a human
// user. See docs/adr/0012-connect-by-identity.md and
// docs/adr/0011-deferred-rbac.md, which fixed this package's shape — a
// session cookie, a fixed role set, and per-node authorization — before it
// was built.
//
// Like [fleet], this package depends on narrow storage interfaces it defines
// itself, not on Postgres: the rules here (which password is valid, which
// role holds which permission) are what an incident review reads to
// understand what authentication and authorization actually enforce.
package user

import (
	"context"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// User is a human-user identity.
type User struct {
	ID           string
	Username     string
	PasswordHash string
	Email        string
	DisabledAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Disabled reports whether this user may no longer authenticate.
func (u User) Disabled() bool { return u.DisabledAt != nil }

// Principal is the minimum identity information the authorization layer
// needs, resolved once per request by the session middleware and carried on
// the request context. It is not [User]: a principal never carries a
// password hash, so it is safe to log or attach to an error's Details.
type Principal struct {
	UserID   string
	Username string
}

// CreateSpec describes a user to create.
type CreateSpec struct {
	Username string
	Password string
	Email    string
}

// minPasswordLength is a floor, not a policy. Atlas has one operator-facing
// credential type today and no password-strength meter to back a more
// elaborate rule; this only stops a trivially empty or one-character
// password from being set.
const minPasswordLength = 8

// Validate reports whether the spec is usable.
func (s CreateSpec) Validate() error {
	const op = "user.CreateSpec.Validate"
	switch {
	case strings.TrimSpace(s.Username) == "":
		return errs.New(errs.CodeInvalidArgument, "a username is required").WithOp(op)
	case len(s.Password) < minPasswordLength:
		return errs.New(errs.CodeInvalidArgument, "password must be at least %d characters", minPasswordLength).WithOp(op)
	}
	return nil
}

// ErrInvalidCredentials is returned by a login attempt that fails for any
// reason — unknown username, wrong password, or a disabled account.
//
// The reason is deliberately not distinguished in the type a caller sees:
// telling an unauthenticated caller *why* a login failed hands an attacker a
// probe for which usernames exist, the same reasoning [fleet.ErrTokenInvalid]
// documents for enrollment tokens.
var ErrInvalidCredentials = errs.New(errs.CodeUnauthenticated, "invalid username or password")

// Store is the storage surface for user identity. Implemented by
// internal/storage/user.Repository.
type Store interface {
	// ByUsername looks up a user by login name, case-insensitively. Returns
	// [errs.CodeNotFound] when none exists.
	ByUsername(ctx context.Context, username string) (User, error)
	// CreateUser persists a new user with an already-hashed password.
	CreateUser(ctx context.Context, u User) error
	// ListUsers returns every user, for operator tooling.
	ListUsers(ctx context.Context) ([]User, error)
}
