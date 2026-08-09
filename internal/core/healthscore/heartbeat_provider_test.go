package healthscore

import (
	"context"
	"testing"
	"time"
)

type fakeNodeHeartbeats struct {
	hb    Heartbeat
	found bool
}

func (f fakeNodeHeartbeats) Heartbeat(context.Context, string) (Heartbeat, bool, error) {
	return f.hb, f.found, nil
}

func TestHeartbeatProviderScoresByStatus(t *testing.T) {
	cases := []struct {
		status string
		want   float64
	}{
		{"up", 100},
		{"stale", 50},
		{"down", 0},
	}
	for _, c := range cases {
		p := HeartbeatProvider{Nodes: fakeNodeHeartbeats{
			hb: Heartbeat{Status: c.status, LastSeenAt: time.Now()}, found: true,
		}}
		sig, err := p.Score(context.Background(), "node-1")
		if err != nil {
			t.Fatalf("score(%s): %v", c.status, err)
		}
		if !sig.Available || sig.Score != c.want {
			t.Fatalf("status %s: got %+v, want score %v", c.status, sig, c.want)
		}
	}
}

func TestHeartbeatProviderUnavailableForUnknownNode(t *testing.T) {
	p := HeartbeatProvider{Nodes: fakeNodeHeartbeats{found: false}}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Available {
		t.Fatal("expected unavailable for a node that has never reported")
	}
}
