package notification

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSendTestSendsEvenWhenChannelIsDisabled covers item 7: a manual test
// must prove the destination and credentials work regardless of whether
// the channel is currently enabled for real traffic — an operator testing
// a channel they just disabled, or have not enabled yet, is exactly the
// case this exists for.
func TestSendTestSendsEvenWhenChannelIsDisabled(t *testing.T) {
	channel := Channel{ID: "c1", Name: "c1", Type: ChannelTeams, Enabled: false, MinSeverity: SeverityCritical}
	channels := newFakeChannelStore(channel)
	deliveries := newFakeDeliveryStore()
	sender := &fakeSender{}
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelTeams: sender}})

	outcome, err := e.SendTest(context.Background(), "c1")
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if !outcome.Delivered || outcome.Error != "" {
		t.Fatalf("outcome = %+v, want Delivered with no error", outcome)
	}
	if sender.calls != 1 {
		t.Errorf("sender called %d times, want 1", sender.calls)
	}

	all := deliveries.all()
	if len(all) != 1 || all[0].Kind != KindTest || all[0].Status != StatusDelivered {
		t.Fatalf("delivery row = %+v, want exactly one, kind=test, delivered", all)
	}
}

// TestSendTestReportsARedactedErrorOnFailure covers the failure half of
// item 7: the outcome's Error is populated, redacted, and the delivery is
// recorded as failed immediately — not queued for the normal retry cycle,
// since a one-off manual test has no "later" to retry at.
func TestSendTestReportsARedactedErrorOnFailure(t *testing.T) {
	channel := Channel{ID: "c1", Name: "c1", Type: ChannelTeams, Enabled: true}
	channels := newFakeChannelStore(channel)
	deliveries := newFakeDeliveryStore()
	sender := &fakeSender{err: errors.New("boom https://evil.example.com/x?sig=verysecret")}
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelTeams: sender}})

	outcome, err := e.SendTest(context.Background(), "c1")
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if outcome.Delivered {
		t.Fatal("expected a failed outcome")
	}
	if outcome.Error == "" {
		t.Fatal("expected a non-empty, redacted error")
	}
	if strings.Contains(outcome.Error, "verysecret") {
		t.Errorf("outcome.Error leaked the secret: %q", outcome.Error)
	}

	all := deliveries.all()
	if len(all) != 1 || all[0].Status != StatusFailed || all[0].Attempts != 1 {
		t.Fatalf("delivery row = %+v, want immediately failed after exactly 1 attempt (no retry)", all)
	}
}

// TestSendTestFailsCleanlyForAnUnknownChannelType covers item 6 through
// the SendTest path specifically.
func TestSendTestFailsCleanlyForAnUnknownChannelType(t *testing.T) {
	channel := Channel{ID: "c1", Name: "c1", Type: ChannelEmail, Enabled: true} // no sender registered below
	channels := newFakeChannelStore(channel)
	deliveries := newFakeDeliveryStore()
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{}})

	outcome, err := e.SendTest(context.Background(), "c1")
	if err != nil {
		t.Fatalf("SendTest: %v", err)
	}
	if outcome.Delivered {
		t.Fatal("expected a failed outcome")
	}
	if !strings.Contains(outcome.Error, "no sender registered") {
		t.Errorf("outcome.Error = %q, want a clear, non-secret message naming the missing sender", outcome.Error)
	}
}

// TestSendTestCanBeRepeated proves two SendTest calls to the same channel
// each actually send — a fixed EventID per channel would collide with
// Enqueue's ON CONFLICT DO NOTHING and silently skip the second insert.
func TestSendTestCanBeRepeated(t *testing.T) {
	channel := Channel{ID: "c1", Name: "c1", Type: ChannelTeams, Enabled: true}
	channels := newFakeChannelStore(channel)
	deliveries := newFakeDeliveryStore()
	sender := &fakeSender{}
	e := NewEngine(Options{Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelTeams: sender}})

	ctx := context.Background()
	if _, err := e.SendTest(ctx, "c1"); err != nil {
		t.Fatalf("SendTest 1: %v", err)
	}
	if _, err := e.SendTest(ctx, "c1"); err != nil {
		t.Fatalf("SendTest 2: %v", err)
	}
	if sender.calls != 2 {
		t.Errorf("sender called %d times, want 2 (one per SendTest call)", sender.calls)
	}
	if len(deliveries.all()) != 2 {
		t.Errorf("delivery rows = %d, want 2 distinct rows", len(deliveries.all()))
	}
}

// blockingSender never returns until release is closed, signalling on
// started the first time Send is called — a stand-in for a destination
// that is slow or hung, to drive [TestStopDoesNotStrandAnInFlightDelivery].
type blockingSender struct {
	started chan struct{}
	release chan struct{}
}

func (b *blockingSender) Send(context.Context, Channel, string, Delivery) error {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return nil
}

// TestStopDoesNotStrandAnInFlightDelivery covers item 4: Stop must return
// within the bound its context gives it even while a send is still
// running, and the row it was mid-sending must not be left claimed
// forever — its lease is simply a future next_attempt_at, the same as any
// other claimed-but-not-yet-finished row, so it becomes due again and
// gets retried once the lease passes, exactly as a crash would leave it.
func TestStopDoesNotStrandAnInFlightDelivery(t *testing.T) {
	channels := newFakeChannelStore(testChannel("c1"))
	deliveries := newFakeDeliveryStore()
	sender := &blockingSender{started: make(chan struct{}, 1), release: make(chan struct{})}
	e := NewEngine(Options{
		Channels: channels, Deliveries: deliveries, Senders: map[ChannelType]Sender{ChannelTeams: sender},
		DispatchInterval: 5 * time.Millisecond,
	})

	ctx := context.Background()
	if err := e.Notify(ctx, Notification{EventID: "evt-1", Trigger: TriggerNodeDown}); err != nil {
		t.Fatalf("notify: %v", err)
	}

	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	go e.Run(runCtx)

	select {
	case <-sender.started:
	case <-time.After(2 * time.Second):
		t.Fatal("the send never started")
	}

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancelStop()
	stopStart := time.Now()
	e.Stop(stopCtx)
	if elapsed := time.Since(stopStart); elapsed > time.Second {
		t.Fatalf("Stop took %v, want it bounded by its own context (100ms)", elapsed)
	}

	all := deliveries.all()
	if len(all) != 1 || all[0].Status != StatusPending {
		t.Fatalf("delivery = %+v, want still pending (claimed, not finished, not stranded as some other state)", all)
	}
	if !all[0].NextAttemptAt.After(time.Now()) {
		t.Errorf("next_attempt_at = %v, want a future lease — a past or zero time would mean it is stuck, "+
			"never reconsidered due again", all[0].NextAttemptAt)
	}

	// Let the blocked send finish so it does not leak past this test, and
	// confirm Run itself does eventually exit once it does.
	close(sender.release)
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after its in-flight send finished")
	}
}
