package app

import (
	"context"
	"net"
	"time"

	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
)

// lazyInstallCodeStore defers repository construction to call time, for the
// same reason as [lazyInventoryStore].
type lazyInstallCodeStore struct{ pool *postgres.Pool }

func (l lazyInstallCodeStore) repo() *storagefleet.Repository {
	return storagefleet.NewRepository(l.pool.DB())
}

func (l lazyInstallCodeStore) CreateInstallCode(ctx context.Context, codeHash string, spec corefleet.InstallCodeSpec, now time.Time) (corefleet.InstallCode, error) {
	return l.repo().CreateInstallCode(ctx, codeHash, spec, now)
}
func (l lazyInstallCodeStore) ListInstallCodes(ctx context.Context) ([]corefleet.InstallCode, error) {
	return l.repo().ListInstallCodes(ctx)
}
func (l lazyInstallCodeStore) RevokeInstallCode(ctx context.Context, id, revokedBy string, now time.Time) (corefleet.InstallCode, error) {
	return l.repo().RevokeInstallCode(ctx, id, revokedBy, now)
}
func (l lazyInstallCodeStore) RedeemInstallCode(ctx context.Context, codeHash, peerID string, sourceIP net.IP, now time.Time) (corefleet.Redemption, error) {
	return l.repo().RedeemInstallCode(ctx, codeHash, peerID, sourceIP, now)
}

// installTargets builds the install command's server-side values from
// configuration, this server's build version, and the running libp2p
// listener's Peer ID. Nothing is auto-detected.
type installTargets struct {
	cfg   *config.Config
	fleet *fleetPipeline
}

func (s installTargets) InstallTarget() (corefleet.InstallTarget, error) {
	const op = "app.installTargets.InstallTarget"
	unavailable := func(msg string) error { return errs.New(errs.CodeUnavailable, "%s", msg).WithOp(op) }

	switch {
	case !s.cfg.Fleet.Enabled || !s.cfg.Fleet.LibP2PEnabled:
		return corefleet.InstallTarget{}, unavailable("install commands need the fleet libp2p listener (ATLAS_FLEET_ENABLED and ATLAS_FLEET_LIBP2P_ENABLED)")
	case s.cfg.PublicHostname == "":
		return corefleet.InstallTarget{}, unavailable("install commands need ATLAS_PUBLIC_HOSTNAME")
	case s.cfg.Fleet.LibP2PAdvertisedAddr == "":
		return corefleet.InstallTarget{}, unavailable("install commands need ATLAS_FLEET_LIBP2P_ADVERTISED_ADDR")
	}
	peerID := s.fleet.ServerPeerID()
	if peerID == "" {
		return corefleet.InstallTarget{}, unavailable("the fleet libp2p listener has not started")
	}
	return corefleet.InstallTarget{
		Version:         build.Current().Version,
		ControlPlaneURL: "https://" + s.cfg.PublicHostname,
		ServerAddr:      s.cfg.Fleet.LibP2PAdvertisedAddr + "/p2p/" + peerID,
	}, nil
}

// ServerPeerID returns this control plane's libp2p Peer ID, or "" when the
// libp2p listener is disabled or not started yet.
func (f *fleetPipeline) ServerPeerID() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.libp2pHost == nil {
		return ""
	}
	return libp2ptransport.PeerID(f.libp2pHost)
}
