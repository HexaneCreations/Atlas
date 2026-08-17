package agent

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/core/transport/remote"
	"github.com/hexane/atlas/internal/core/transport/spool"
)

// healthyTelemetryServer answers exactly like the real agent-facing
// telemetry endpoint's happy path: accept, decode, ack.
func healthyTelemetryServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Envelopes []transport.Envelope `json:"envelopes"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": len(req.Envelopes), "rejected": nil, "directive": "ok",
		})
	}))
}

// unreachableURL returns a URL nothing is listening on: bind a listener,
// close it immediately, and hand back its address. Connections to it are
// refused deterministically instead of relying on a hardcoded port that
// might be in use.
func unreachableURL(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return "http://" + addr
}

func buildTarget(t *testing.T, id, baseURL string) fanoutTarget {
	t.Helper()
	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	tr, err := remote.New(remote.Options{BaseURL: baseURL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	t.Cleanup(func() { tr.Close() })
	return fanoutTarget{id: id, environment: id, tr: tr}
}

func sendSample(t *testing.T, fanout *fanoutTransport) {
	t.Helper()
	batch := collect.Batch{CollectorID: "system.cpu", Samples: []collect.Sample{
		{Metric: "cpu.percent", Value: 42, Unit: collect.UnitPercent, Kind: collect.KindGauge, Time: time.Now()},
	}}
	env := transport.NewEnvelopeOf(transport.Origin{NodeID: "node-1"}, transport.Metrics{Batch: batch})
	if err := fanout.Send(context.Background(), env); err != nil {
		t.Fatalf("fanout.Send: %v", err)
	}
}

// waitForStats polls until cond(remote.Transport.Stats()) is true or the
// deadline passes — the replay loop runs on its own goroutine, so a
// just-spooled envelope is not necessarily retried and failed yet.
func waitForStats(t *testing.T, tr *remote.Transport, cond func(remote.Stats) bool) remote.Stats {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		s := tr.Stats()
		if cond(s) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met before deadline; last stats = %+v", s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Direct evidence for goal §5, case 1: local healthy, production
// unavailable. Local must keep delivering; production must retry/fail
// independently; neither transport's state leaks into the other's.
func TestLocalVerifyRelationshipIsolation_LocalHealthy_ProductionDown(t *testing.T) {
	local := healthyTelemetryServer(t)
	defer local.Close()

	localTarget := buildTarget(t, "local", local.URL)
	prodTarget := buildTarget(t, "production", unreachableURL(t))
	fanout := newFanoutTransport([]fanoutTarget{localTarget, prodTarget})

	sendSample(t, fanout)
	sendSample(t, fanout)

	localStats := waitForStats(t, localTarget.tr.(*remote.Transport), func(s remote.Stats) bool { return s.Sent >= 2 })
	if localStats.Failed != 0 {
		t.Errorf("local (healthy) recorded %d failures, want 0", localStats.Failed)
	}

	prodStats := waitForStats(t, prodTarget.tr.(*remote.Transport), func(s remote.Stats) bool { return s.Retries > 0 })
	if prodStats.Sent != 0 {
		t.Errorf("production (down) recorded %d sent, want 0", prodStats.Sent)
	}
	if prodStats.Spooled == 0 {
		t.Error("production (down) has nothing spooled — envelopes were lost, not queued for retry")
	}

	t.Logf("local:      %+v", localStats)
	t.Logf("production: %+v", prodStats)
}

// The mirror image: production healthy, local unavailable.
func TestLocalVerifyRelationshipIsolation_ProductionHealthy_LocalDown(t *testing.T) {
	prod := healthyTelemetryServer(t)
	defer prod.Close()

	localTarget := buildTarget(t, "local", unreachableURL(t))
	prodTarget := buildTarget(t, "production", prod.URL)
	fanout := newFanoutTransport([]fanoutTarget{localTarget, prodTarget})

	sendSample(t, fanout)
	sendSample(t, fanout)

	prodStats := waitForStats(t, prodTarget.tr.(*remote.Transport), func(s remote.Stats) bool { return s.Sent >= 2 })
	if prodStats.Failed != 0 {
		t.Errorf("production (healthy) recorded %d failures, want 0", prodStats.Failed)
	}

	localStats := waitForStats(t, localTarget.tr.(*remote.Transport), func(s remote.Stats) bool { return s.Retries > 0 })
	if localStats.Sent != 0 {
		t.Errorf("local (down) recorded %d sent, want 0", localStats.Sent)
	}
	if localStats.Spooled == 0 {
		t.Error("local (down) has nothing spooled — envelopes were lost, not queued for retry")
	}

	t.Logf("production: %+v", prodStats)
	t.Logf("local:      %+v", localStats)
}
