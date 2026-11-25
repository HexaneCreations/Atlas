//go:build integration

package notification_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	corenotification "github.com/hexane/atlas/internal/core/notification"
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
