package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// connectedAgent builds a server host for f and an agent host dialled into
// it, recorded as node-a.
func connectedAgent(t *testing.T, f *fleetPipeline) host.Host {
	t.Helper()
	server, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	agent, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close() })
	target, err := libp2ptransport.ParseTarget(libp2ptransport.Addrs(server)[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Connect(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	f.libp2pHost = server
	f.peerByNode["node-a"] = agent.ID()
	return agent
}

// versionOneAgent answers exactly as an Agent released before version 2:
// any request version but 1 gets "unsupported protocol version N".
func versionOneAgent(h host.Host, seen chan<- libp2ptransport.AgentOpRequest) {
	h.SetStreamHandler(libp2ptransport.AgentOpsProtocolID, func(s network.Stream) {
		defer s.Close()
		var req libp2ptransport.AgentOpRequest
		if json.NewDecoder(s).Decode(&req) != nil {
			return
		}
		seen <- req
		enc := json.NewEncoder(s)
		if req.ProtocolVersion != 1 {
			_ = enc.Encode(libp2ptransport.AgentOpFrame{Type: "error", Reason: fmt.Sprintf("unsupported protocol version %d", req.ProtocolVersion)})
			return
		}
		_ = enc.Encode(libp2ptransport.AgentOpFrame{Type: "line", Time: time.Now(), Stream: "stdout", Message: "hello"})
		_ = enc.Encode(libp2ptransport.AgentOpFrame{Type: "end"})
	})
}

func drainLogs(lines <-chan docker.LogLine, errCh <-chan error) ([]docker.LogLine, error) {
	var out []docker.LogLine
	for l := range lines {
		out = append(out, l)
	}
	return out, <-errCh
}

func TestFleetLearnsAVersionOneAgentCannotEndAtUntil(t *testing.T) {
	f, _ := newTestFleetPipeline(t)
	agent := connectedAgent(t, f)
	seen := make(chan libp2ptransport.AgentOpRequest, 4)
	versionOneAgent(agent, seen)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if !f.SupportsUntil("node-a") {
		t.Fatal("an agent not yet asked counts as unable")
	}
	since := time.Date(2026, 5, 1, 10, 0, 0, 500_000_000, time.UTC)
	_, err := drainLogs(f.ContainerLogs(ctx, "node-a", "c1", docker.LogOptions{Since: since, Until: since.Add(time.Hour)}))
	if !errors.Is(err, docker.ErrUntilUnsupported) {
		t.Fatalf("err = %v, want ErrUntilUnsupported", err)
	}
	if req := <-seen; req.ProtocolVersion != 2 || req.Until == "" || req.Since != "2026-05-01T10:00:00.5Z" {
		t.Errorf("request = %+v", req)
	}
	if f.SupportsUntil("node-a") {
		t.Error("the refusal was not remembered")
	}

	// Without until the request is version 1, which this agent serves.
	lines, err := drainLogs(f.ContainerLogs(ctx, "node-a", "c1", docker.LogOptions{Since: since}))
	if err != nil || len(lines) != 1 {
		t.Fatalf("version 1 read = %v, %v", lines, err)
	}
	if req := <-seen; req.ProtocolVersion != 1 || req.Until != "" {
		t.Errorf("an open-ended read sent %+v", req)
	}
}

func TestFleetPassesUntilToAVersionTwoAgent(t *testing.T) {
	f, _ := newTestFleetPipeline(t)
	agent := connectedAgent(t, f)
	got := make(chan time.Time, 1)
	libp2ptransport.RegisterAgentOpsHandler(agent, func(_ context.Context, _ string, _ int, _, until time.Time, _, _ bool) (<-chan libp2ptransport.LogLine, <-chan error, error) {
		got <- until
		out := make(chan libp2ptransport.LogLine)
		close(out)
		errCh := make(chan error)
		close(errCh)
		return out, errCh, nil
	}, nil, func(peer.ID) (bool, bool) { return true, true })
	f.setSupportsUntil("node-a", false)
	f.noUntil["node-a"] = time.Now().Add(-time.Hour) // the refusal has aged out: ask again

	until := time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !f.SupportsUntil("node-a") {
		t.Fatal("an aged refusal still counts")
	}
	if _, err := drainLogs(f.ContainerLogs(ctx, "node-a", "c1", docker.LogOptions{Until: until})); err != nil {
		t.Fatal(err)
	}
	if u := <-got; !u.Equal(until) {
		t.Errorf("agent saw until %v", u)
	}
	if _, refused := f.noUntil["node-a"]; refused {
		t.Error("a served version 2 request did not clear the refusal")
	}
}
