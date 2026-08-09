package healthscore

import (
	"context"
	"fmt"
	"time"
)

// Heartbeat is a node's liveness classification. Values mirror
// [github.com/hexane/atlas/internal/storage/metric.NodeStatus] ("up",
// "stale", "down") so this package stays free of a storage dependency; the
// adapter in internal/app does the conversion, reusing the same
// classification the node API already surfaces rather than defining a
// second one.
type Heartbeat struct {
	Status     string
	LastSeenAt time.Time
}

// NodeHeartbeats resolves a node's liveness. Satisfied via an adapter over
// [github.com/hexane/atlas/internal/storage/metric.Repository.GetNode]; see
// internal/app.
type NodeHeartbeats interface {
	// Heartbeat returns a node's liveness, or found=false if the node has
	// never been observed.
	Heartbeat(ctx context.Context, nodeID string) (Heartbeat, bool, error)
}

// HeartbeatProvider scores a node on how recently it reported.
type HeartbeatProvider struct {
	Nodes NodeHeartbeats
}

func (p HeartbeatProvider) Name() string { return "heartbeat" }

func (p HeartbeatProvider) Score(ctx context.Context, nodeID string) (Signal, error) {
	hb, found, err := p.Nodes.Heartbeat(ctx, nodeID)
	if err != nil {
		return Signal{}, err
	}
	if !found {
		return Signal{Available: false, Detail: "node has never reported"}, nil
	}

	var score float64
	switch hb.Status {
	case "up":
		score = 100
	case "stale":
		score = 50
	default:
		score = 0
	}
	return Signal{
		Score: score, Available: true,
		Detail: fmt.Sprintf("%s, last seen %s ago", hb.Status, time.Since(hb.LastSeenAt).Round(time.Second)),
	}, nil
}
