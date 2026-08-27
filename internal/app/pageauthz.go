package app

import (
	"context"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
)

// lazyPageAuthorizer defers repository construction to call time, the same
// reason and shape as [lazyAuthorizer] — the page-visibility layer is a
// second, independent authorization axis (see internal/core/pageauthz's
// doc), not an extension of the operation-level one lazyAuthorizer serves.
type lazyPageAuthorizer struct{ pool *postgres.Pool }

func (l lazyPageAuthorizer) Require(ctx context.Context, userID string, page corepageauthz.Page, nodeID string) error {
	return corepageauthz.NewAuthorizer(storagepageauthz.NewRepository(l.pool.DB())).Require(ctx, userID, page, nodeID)
}

// lazyPageAdmin exposes the full page-access management surface — bundle
// definitions, assignments, and direct grants — for the admin Users page,
// the same "one repository, several narrow interfaces" shape
// [lazyUserStore] already uses.
type lazyPageAdmin struct{ pool *postgres.Pool }

func (l lazyPageAdmin) repo() *storagepageauthz.Repository {
	return storagepageauthz.NewRepository(l.pool.DB())
}

func (l lazyPageAdmin) CreateRoleAccess(ctx context.Context, name, createdBy string, now time.Time) error {
	return l.repo().CreateRoleAccess(ctx, name, createdBy, now)
}
func (l lazyPageAdmin) AddPageToRoleAccess(ctx context.Context, roleAccessName string, page corepageauthz.Page) error {
	return l.repo().AddPageToRoleAccess(ctx, roleAccessName, page)
}
func (l lazyPageAdmin) RemovePageFromRoleAccess(ctx context.Context, roleAccessName string, page corepageauthz.Page) error {
	return l.repo().RemovePageFromRoleAccess(ctx, roleAccessName, page)
}
func (l lazyPageAdmin) ListRoleAccessDefinitions(ctx context.Context) ([]corepageauthz.RoleAccess, error) {
	return l.repo().ListRoleAccessDefinitions(ctx)
}
func (l lazyPageAdmin) AssignRoleAccess(ctx context.Context, spec corepageauthz.RoleAccessAssignmentSpec, now time.Time) error {
	return l.repo().AssignRoleAccess(ctx, spec, now)
}
func (l lazyPageAdmin) RevokeRoleAccessAssignment(ctx context.Context, assignmentID, revokedBy string, now time.Time) error {
	return l.repo().RevokeRoleAccessAssignment(ctx, assignmentID, revokedBy, now)
}
func (l lazyPageAdmin) ListRoleAccessAssignments(ctx context.Context, userID string) ([]corepageauthz.RoleAccessAssignment, error) {
	return l.repo().ListRoleAccessAssignments(ctx, userID)
}
func (l lazyPageAdmin) GrantPageAccess(ctx context.Context, spec corepageauthz.PageGrantSpec, now time.Time) error {
	return l.repo().GrantPageAccess(ctx, spec, now)
}
func (l lazyPageAdmin) RevokePageAccess(ctx context.Context, grantID, revokedBy string, now time.Time) error {
	return l.repo().RevokePageAccess(ctx, grantID, revokedBy, now)
}
func (l lazyPageAdmin) ListPageAccessGrants(ctx context.Context, userID string) ([]corepageauthz.PageGrant, error) {
	return l.repo().ListPageAccessGrants(ctx, userID)
}
