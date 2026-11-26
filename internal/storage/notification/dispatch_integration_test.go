//go:build integration

package notification_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/storage/metric"
)

// countingSender counts how many times Send actually runs — the thing
// that must happen exactly once per delivery no matter how many claimers
// race for it.
type countingSender struct {
	calls atomic.Int32
}

func (s *countingSender) Send(context.Context, corenotification.Channel, string, corenotification.Delivery) error {
	s.calls.Add(1)
	return nil
}

// TestConcurrentWorkersClaimADueRowExactlyOnce is item 2's required test:
// two workers — here, two independent *Engines sharing one database, the
// same shape two separate server processes would have — race to claim
// the same due row. Before ClaimDue's single atomic SELECT ... FOR UPDATE
// SKIP LOCKED + UPDATE, a naive read-then-write claim lets both read the
// row before either writes, and both send it; this asserts exactly one
// send happened, which only holds with the atomic claim in place.
func TestConcurrentWorkersClaimADueRowExactlyOnce(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()

	// A channel type the real senders never see — a bare fake stands in
	// for "teams" or "webhook" here, since this test is about claiming,
	// not about any particular sender's wire format.
	c, err := repo.CreateChannel(ctx, channelInput("race", ptr("dummy-secret")))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if _, err := repo.Enqueue(ctx, notificationFor("race-evt"), []corenotification.Channel{c}); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	sender := &countingSender{}
	senders := map[corenotification.ChannelType]corenotification.Sender{corenotification.ChannelEmail: sender}
	worker1 := corenotification.NewEngine(corenotification.Options{Channels: repo, Deliveries: repo, Senders: senders})
	worker2 := corenotification.NewEngine(corenotification.Options{Channels: repo, Deliveries: repo, Senders: senders})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); worker1.DispatchOnce(ctx) }()
	go func() { defer wg.Done(); worker2.DispatchOnce(ctx) }()
	wg.Wait()

	// Run a second round in case the race did not land on the first try —
	// SKIP LOCKED means an unlucky ordering could have one worker's claim
	// query simply see nothing to claim rather than contend for the row,
	// which still proves safety but would not exercise the contended path
	// this test exists to check. A fixed single row makes the contended
	// path the common one, but a retry costs nothing and removes any doubt.
	if sender.calls.Load() == 0 {
		wg.Add(2)
		go func() { defer wg.Done(); worker1.DispatchOnce(ctx) }()
		go func() { defer wg.Done(); worker2.DispatchOnce(ctx) }()
		wg.Wait()
	}

	if got := sender.calls.Load(); got != 1 {
		t.Fatalf("Send was called %d times for one delivery, want exactly 1", got)
	}
}

// nodeInfoFromMetric resolves nodes the way the server does: hostname and
// observed public address from the nodes table.
type nodeInfoFromMetric struct{ repo *metric.Repository }

func (a nodeInfoFromMetric) NodeInfo(ctx context.Context, nodeID string) (string, string, error) {
	n, err := a.repo.GetNode(ctx, nodeID)
	if err != nil {
		return "", "", err
	}
	return n.Hostname, n.PublicIP, nil
}

type nodeRecordingSender struct {
	mu   sync.Mutex
	sent []corenotification.Delivery
}

func (s *nodeRecordingSender) Send(_ context.Context, _ corenotification.Channel, _ string, d corenotification.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, d)
	return nil
}

func TestDispatchResolvesTheNodeAtSendTimeAndNeverStoresIt(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	metrics := metric.NewRepository(pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ('node-1', 'web-01', now())`); err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := metrics.UpdatePublicIP(ctx, "node-1", "203.0.113.7"); err != nil {
		t.Fatalf("UpdatePublicIP: %v", err)
	}

	c, err := repo.CreateChannel(ctx, channelInput("named", ptr("dummy-secret")))
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	known := notificationFor("named-evt")
	unknown := notificationFor("unknown-evt")
	unknown.NodeID = "node-gone"
	for _, n := range []corenotification.Notification{known, unknown} {
		if _, err := repo.Enqueue(ctx, n, []corenotification.Channel{c}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	sender := &nodeRecordingSender{}
	engine := corenotification.NewEngine(corenotification.Options{
		Channels: repo, Deliveries: repo, NodeInfo: nodeInfoFromMetric{repo: metrics},
		Senders: map[corenotification.ChannelType]corenotification.Sender{corenotification.ChannelEmail: sender},
	})
	engine.DispatchOnce(ctx)

	got := map[string][2]string{}
	for _, d := range sender.sent {
		got[d.NodeID] = [2]string{d.NodeName, d.NodeAddress}
	}
	if len(sender.sent) != 2 || got["node-1"] != [2]string{"web-01", "203.0.113.7"} || got["node-gone"] != [2]string{"", ""} {
		t.Fatalf("sent = %+v, want node-1 named and node-gone sent with its id only", sender.sent)
	}

	var columns []string
	rows, err := pool.Query(ctx, `SELECT column_name FROM information_schema.columns
		WHERE table_name = 'notification_deliveries' ORDER BY column_name`)
	if err != nil {
		t.Fatalf("list columns: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns = append(columns, name)
	}
	want := "attempts,channel_id,channel_type,created_at,delivered_at,event_id,id,kind,last_error,message," +
		"next_attempt_at,node_id,severity,status,subject,title,trigger,updated_at"
	if got := strings.Join(columns, ","); got != want {
		t.Errorf("notification_deliveries columns = %s, want %s (node details must not be stored)", got, want)
	}

	page, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListDeliveries: %v", err)
	}
	for _, d := range page.Deliveries {
		if d.Status != corenotification.StatusDelivered || d.NodeName != "" || d.NodeAddress != "" {
			t.Errorf("stored delivery = %+v, want delivered with no node name or address", d)
		}
	}
}
