package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	p2phost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// cachedTarget is the last rendezvous [libp2ptransport.LookupResult] this
// Agent successfully resolved, persisted so a restart (or a transient relay
// outage) can still dial the control plane without a fresh lookup.
type cachedTarget struct {
	DirectAddrs []string `json:"direct_addrs"`
	CircuitAddr string   `json:"circuit_addr"`
}

func cachedTargetPath(dataDir string) string {
	return filepath.Join(dataDir, "p2p-last-known.json")
}

func loadCachedTarget(dataDir string) (cachedTarget, bool) {
	data, err := os.ReadFile(cachedTargetPath(dataDir))
	if err != nil {
		return cachedTarget{}, false
	}
	var t cachedTarget
	if err := json.Unmarshal(data, &t); err != nil {
		return cachedTarget{}, false
	}
	return t, true
}

func saveCachedTarget(dataDir string, t cachedTarget) error {
	data, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return os.WriteFile(cachedTargetPath(dataDir), data, 0o600)
}

// buildCandidates turns a rendezvous result's raw address strings into
// dialable [peer.AddrInfo] values, in the order [libp2ptransport.DialWithFallback]
// should try them: direct addresses first, the relay circuit address last.
// Anything that doesn't parse, or doesn't actually name serverID, is
// dropped rather than failing the whole lookup — a relay register cannot be
// trusted to be perfectly self-consistent.
func buildCandidates(directAddrs []string, circuitAddr string, serverID peer.ID) ([]peer.AddrInfo, error) {
	var out []peer.AddrInfo
	for _, a := range directAddrs {
		info, err := libp2ptransport.ParseTarget(a)
		if err != nil || info.ID != serverID {
			continue
		}
		out = append(out, info)
	}
	if circuitAddr != "" {
		if info, err := libp2ptransport.ParseTarget(circuitAddr); err == nil && info.ID == serverID {
			out = append(out, info)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid candidate addresses for peer %s", serverID)
	}
	return out, nil
}

// lookupAndCache asks relay for serverID's current addresses and persists
// the result to dataDir on success.
func lookupAndCache(ctx context.Context, h p2phost.Host, relayInfo peer.AddrInfo, serverID peer.ID, dataDir string) ([]peer.AddrInfo, error) {
	res, err := libp2ptransport.Lookup(ctx, h, relayInfo, serverID)
	if err != nil {
		return nil, fmt.Errorf("rendezvous lookup: %w", err)
	}
	if !res.Found {
		return nil, fmt.Errorf("relay has no announced address for peer %s", serverID)
	}
	candidates, err := buildCandidates(res.DirectAddrs, res.CircuitAddr, serverID)
	if err != nil {
		return nil, err
	}
	if err := saveCachedTarget(dataDir, cachedTarget{DirectAddrs: res.DirectAddrs, CircuitAddr: res.CircuitAddr}); err != nil {
		// Non-fatal: caching is a resilience aid, not required for this dial.
		return candidates, nil
	}
	return candidates, nil
}

// resolveCandidates looks the control plane up via relay, falling back to
// the last cached result (from a previous successful lookup) if the relay
// is unreachable or has no record — e.g. a temporary relay outage should
// not prevent an Agent that has already contacted the Server once from
// reconnecting.
func resolveCandidates(ctx context.Context, h p2phost.Host, relayInfo peer.AddrInfo, serverID peer.ID, dataDir string, logger *slog.Logger) ([]peer.AddrInfo, error) {
	candidates, err := lookupAndCache(ctx, h, relayInfo, serverID, dataDir)
	if err == nil {
		return candidates, nil
	}
	logger.WarnContext(ctx, "rendezvous lookup failed, trying cached address",
		slog.String("error", err.Error()))

	cached, ok := loadCachedTarget(dataDir)
	if !ok {
		return nil, fmt.Errorf("rendezvous lookup failed and no cached address is available: %w", err)
	}
	return buildCandidates(cached.DirectAddrs, cached.CircuitAddr, serverID)
}

// newDiscoveryDial returns the dialContextFunc for the rendezvous discovery
// path: resolve candidates (live lookup, cache fallback), dial direct-first
// with a relay fallback, and — if that whole attempt fails — force one
// fresh lookup (bypassing the cache, which may be what's stale) before
// giving up, in case the Server's address changed since the last lookup.
func newDiscoveryDial(h p2phost.Host, relayInfo peer.AddrInfo, serverID peer.ID, dataDir string, logger *slog.Logger) dialContextFunc {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		candidates, err := resolveCandidates(ctx, h, relayInfo, serverID, dataDir, logger)
		if err != nil {
			return nil, fmt.Errorf("resolve control plane address: %w", err)
		}

		conn, dialErr := libp2ptransport.DialWithFallback(ctx, h, candidates)
		if dialErr == nil {
			return conn, nil
		}

		fresh, lookupErr := lookupAndCache(ctx, h, relayInfo, serverID, dataDir)
		if lookupErr != nil {
			return nil, dialErr
		}
		return libp2ptransport.DialWithFallback(ctx, h, fresh)
	}
}
