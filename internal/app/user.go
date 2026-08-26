package app

import (
	"context"
	"time"

	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/postgres"
	storageuser "github.com/hexane/atlas/internal/storage/user"
)

// lazyUserStore defers repository construction to call time, for the same
// reason as [lazyInventoryStore]. One repository backs all three of the
// user domain's storage interfaces (identity, sessions, node-role grants),
// the same way [storageuser.Repository] implements all three — see its doc.
type lazyUserStore struct{ pool *postgres.Pool }

func (l lazyUserStore) repo() *storageuser.Repository { return storageuser.NewRepository(l.pool.DB()) }

func (l lazyUserStore) ByUsername(ctx context.Context, username string) (coreuser.User, error) {
	return l.repo().ByUsername(ctx, username)
}

func (l lazyUserStore) CreateSession(ctx context.Context, s coreuser.Session) error {
	return l.repo().CreateSession(ctx, s)
}
func (l lazyUserStore) Resolve(ctx context.Context, tokenHash string, now time.Time) (coreuser.Principal, error) {
	return l.repo().Resolve(ctx, tokenHash, now)
}
func (l lazyUserStore) RevokeSession(ctx context.Context, tokenHash string, now time.Time) error {
	return l.repo().RevokeSession(ctx, tokenHash, now)
}
func (l lazyUserStore) RevokeAllSessions(ctx context.Context, userID, actorUserID string, now time.Time) error {
	return l.repo().RevokeAllSessions(ctx, userID, actorUserID, now)
}

// The admin Users page's surface — see [v1.UserAdmin].
func (l lazyUserStore) ListUsersWithGrants(ctx context.Context) ([]coreuser.UserWithGrants, error) {
	return l.repo().ListUsersWithGrants(ctx)
}
func (l lazyUserStore) GetUser(ctx context.Context, id string) (coreuser.User, error) {
	return l.repo().GetUser(ctx, id)
}
func (l lazyUserStore) AdminCreateUser(ctx context.Context, username, email, actorUserID string, now time.Time) (coreuser.User, string, error) {
	return l.repo().AdminCreateUser(ctx, username, email, actorUserID, now)
}
func (l lazyUserStore) DisableUser(ctx context.Context, userID, actorUserID string, now time.Time) error {
	return l.repo().DisableUser(ctx, userID, actorUserID, now)
}
func (l lazyUserStore) EnableUser(ctx context.Context, userID, actorUserID string, now time.Time) error {
	return l.repo().EnableUser(ctx, userID, actorUserID, now)
}
func (l lazyUserStore) ResetPassword(ctx context.Context, userID, actorUserID string, now time.Time) (string, error) {
	return l.repo().ResetPassword(ctx, userID, actorUserID, now)
}
func (l lazyUserStore) ListAudit(ctx context.Context, targetUserID string) ([]coreuser.AuditEntry, error) {
	return l.repo().ListAudit(ctx, targetUserID)
}
func (l lazyUserStore) Grant(ctx context.Context, spec coreuser.GrantSpec, now time.Time) error {
	return l.repo().Grant(ctx, spec, now)
}
func (l lazyUserStore) RevokeGrant(ctx context.Context, grantID, revokedBy string, now time.Time) error {
	return l.repo().RevokeGrant(ctx, grantID, revokedBy, now)
}

// lazyAuthorizer wraps [lazyUserStore] in [coreuser.Authorizer], the
// handler-facing policy layer — see internal/api/v1's requireScope.
type lazyAuthorizer struct{ pool *postgres.Pool }

func (l lazyAuthorizer) Require(ctx context.Context, principal coreuser.Principal, permission coreuser.Permission, nodeID string) error {
	return coreuser.NewAuthorizer(storageuser.NewRepository(l.pool.DB())).Require(ctx, principal, permission, nodeID)
}

func (l lazyAuthorizer) AuthorizedNodes(ctx context.Context, principal coreuser.Principal, permission coreuser.Permission) (bool, map[string]bool, error) {
	return coreuser.NewAuthorizer(storageuser.NewRepository(l.pool.DB())).AuthorizedNodes(ctx, principal, permission)
}
