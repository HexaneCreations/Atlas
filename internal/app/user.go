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

// lazyAuthorizer wraps [lazyUserStore] in [coreuser.Authorizer], the
// handler-facing policy layer — see internal/api/v1's requireScope.
type lazyAuthorizer struct{ pool *postgres.Pool }

func (l lazyAuthorizer) Require(ctx context.Context, principal coreuser.Principal, permission coreuser.Permission, nodeID string) error {
	return coreuser.NewAuthorizer(storageuser.NewRepository(l.pool.DB())).Require(ctx, principal, permission, nodeID)
}
