package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/plugin/system"
	"github.com/hexane/atlas/internal/storage/metric"
)

// snapshotPromoter is the remote sibling of the system plugin's host
// collector: when a "host" or "network" inventory snapshot arrives from a
// remote agent, it promotes the same facts the local collector writes into
// the nodes table, so nodes.os/platform/... and node_addresses reflect a
// remote node exactly as they do a locally monitored one.
//
// It lives in internal/app because decoding the payload needs the concrete
// plugin types, and app is already the layer that knows which plugin type
// each inventory subject carries — internal/storage/inventory stays generic.
type snapshotPromoter struct{ repo *metric.Repository }

func (p snapshotPromoter) PromoteHost(ctx context.Context, env transport.Envelope, data json.RawMessage) error {
	const op = "app.snapshotPromoter.PromoteHost"

	var info system.HostInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not decode a host inventory snapshot").WithOp(op)
	}

	if err := p.repo.EnsureNode(ctx, env); err != nil {
		return err
	}
	// Same mapping the local path applies in system.hostCollector.Collect.
	return p.repo.UpdateNodeFacts(ctx, env.Origin.NodeID, metric.NodeFacts{
		OS:           info.OS,
		Platform:     system.PlatformLabel(info),
		Kernel:       info.KernelVersion,
		Architecture: info.KernelArch,
		CPUCores:     info.LogicalCores,
		BootTime:     info.BootTime,
		HardwareUUID: info.HardwareUUID,
	})
}

func (p snapshotPromoter) PromoteNetwork(ctx context.Context, env transport.Envelope, observedAt time.Time, data json.RawMessage) error {
	const op = "app.snapshotPromoter.PromoteNetwork"

	var ni system.NetworkIdentity
	if err := json.Unmarshal(data, &ni); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not decode a network inventory snapshot").WithOp(op)
	}

	var addrs []metric.NodeAddress
	for _, iface := range ni.Interfaces {
		for _, cidr := range iface.IPv4 {
			addrs = append(addrs, metric.NodeAddress{Interface: iface.Name, Address: cidr})
		}
		for _, cidr := range iface.IPv6 {
			addrs = append(addrs, metric.NodeAddress{Interface: iface.Name, Address: cidr})
		}
	}

	if err := p.repo.EnsureNode(ctx, env); err != nil {
		return err
	}
	return p.repo.ReplaceNodeAddresses(ctx, env.Origin.NodeID, observedAt, addrs)
}
