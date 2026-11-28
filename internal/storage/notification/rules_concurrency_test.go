//go:build integration

package notification_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/storage/notification"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// delayingTracer holds open whatever transaction runs the one query whose
// SQL contains match, for delay, and signals fired (once) right after that
// query returns — so a second, concurrent goroutine can be released to run
// exactly while the first transaction is still open and still holding
// whatever locks that query took.
type delayingTracer struct {
	match string
	delay time.Duration
	once  sync.Once
	fired chan struct{}
}

type tracerSQLKey struct{}

func (tr *delayingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, tracerSQLKey{}, data.SQL)
}

func (tr *delayingTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	sql, _ := ctx.Value(tracerSQLKey{}).(string)
	if !strings.Contains(sql, tr.match) {
		return
	}
	tr.once.Do(func() { close(tr.fired) })
	time.Sleep(tr.delay)
}

// TestSetRuleRejectsAGlobalChangeRacingAConcurrentOverride is fix 4's
// concurrency test. It forces the exact interleaving that corrupts state
// without the per-trigger advisory lock:
//
//  1. An override SetRule call (for TriggerDiskPct, a new node) reads the
//     global row (SELECT ... FOR SHARE) and is then held open for a bit by
//     a query tracer, simulating however long a real call might take
//     between that read and its own write.
//  2. While it is held open, a second, independent SetRule call changes
//     the global default to a crit threshold lower than the override's
//     warn threshold — a combination [corenotification.MergeRule] rejects.
//
// Without the lock, the global call's own read of the existing overrides
// (FOR UPDATE) runs before the override has committed, sees none, and
// validates and commits the incompatible global unopposed: both calls
// return success, and the two rows left behind are individually valid but
// jointly inconsistent. With the lock, the global call blocks until the
// override commits, then reads the just-committed override and correctly
// rejects itself as incompatible — so this test fails on
// internal/storage/notification/rules.go before the
// pg_advisory_xact_lock fix and passes after it.
func TestSetRuleRejectsAGlobalChangeRacingAConcurrentOverride(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "race-node")

	tracedCfg := pool.Config()
	tracer := &delayingTracer{match: "monitor_id IS NULL) FOR SHARE", delay: 300 * time.Millisecond, fired: make(chan struct{})}
	tracedCfg.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(ctx, tracedCfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	defer tracedPool.Close()
	tracedRepo := notification.NewRepository(tracedPool, nil)

	var wg sync.WaitGroup
	var overrideErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		overrideErr = tracedRepo.SetRule(ctx, corenotification.RuleSpec{
			Trigger: corenotification.TriggerDiskPct, NodeID: "race-node", WarnThreshold: ptr(85.0),
		}, "override-caller")
	}()

	select {
	case <-tracer.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the tracer never observed the override's global-read query; its SQL text may have changed")
	}

	globalErr := repo.SetRule(ctx, corenotification.RuleSpec{
		Trigger: corenotification.TriggerDiskPct, Enabled: ptr(true), WarnThreshold: ptr(70.0), CritThreshold: ptr(80.0),
		ForSeconds: ptr(300), RecoverySeconds: ptr(120), Hysteresis: ptr(5.0), CooldownSeconds: ptr(1800),
		SendRecovery: ptr(true), SubjectExcludes: []string{},
	}, "global-caller")

	wg.Wait()

	if overrideErr != nil {
		t.Fatalf("override SetRule: %v, want it to succeed (warn 85 is valid against the original crit 90)", overrideErr)
	}
	if globalErr == nil {
		t.Fatal("expected the concurrent global change (crit 80) to be rejected as incompatible with the " +
			"in-flight override (warn 85), got nil — the two rows were committed without ever being validated against each other")
	}
	if !strings.Contains(globalErr.Error(), "conflicts with the override") {
		t.Errorf("globalErr = %v, want it to name the override conflict", globalErr)
	}

	r, err := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "")
	if err != nil {
		t.Fatalf("EffectiveRule(global): %v", err)
	}
	if *r.CritThreshold != 90 {
		t.Errorf("global crit_threshold = %v, want it unchanged at 90 (the rejected write must not have applied)", *r.CritThreshold)
	}
}
