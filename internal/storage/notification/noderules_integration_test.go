//go:build integration

package notification_test

import (
	"context"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Every catalog rule must have a seeded global row whose enabled matches the
// catalog default. A new trigger without a seed migration fails here.
func TestCatalogMatchesTheSeededGlobalRules(t *testing.T) {
	repo, _ := newRepository(t)
	specs, err := repo.ListRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	global := map[corenotification.Trigger]corenotification.RuleSpec{}
	for _, s := range specs {
		if s.IsGlobal() {
			global[s.Trigger] = s
		}
	}
	for _, entry := range corenotification.Catalog {
		g, ok := global[entry.Key]
		if !ok {
			t.Fatalf("catalog rule %q has no global rule row: add a migration seeding it", entry.Key)
		}
		if g.Enabled == nil || *g.Enabled != entry.DefaultEnabled {
			t.Errorf("catalog rule %q: default_enabled %v, seeded enabled %v", entry.Key, entry.DefaultEnabled, g.Enabled)
		}
	}
	for trigger := range global {
		if _, ok := corenotification.CatalogEntryFor(trigger); !ok {
			t.Errorf("global rule %q is not in the catalog", trigger)
		}
	}
}

func firingState(nodeID string, trigger corenotification.Trigger, subject string) corenotification.State {
	now := time.Now()
	return corenotification.State{
		Trigger: trigger, NodeID: nodeID, Subject: subject, State: corenotification.StateFiring,
		Severity: corenotification.SeverityCritical, ConditionSince: &now, StateSince: now,
		Baseline: map[string]any{"seen_up": true, "other": "kept"},
	}
}

func TestDisablingANodeRuleResetsItsStateInTheSameWrite(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	seedNode(t, pool, "node-2")
	for _, s := range []corenotification.State{
		firingState("node-1", corenotification.TriggerContainerExit, "web"),
		firingState("node-1", corenotification.TriggerDiskPct, "/"),
		firingState("node-2", corenotification.TriggerContainerExit, "web"),
	} {
		if err := repo.UpsertState(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	rule, err := repo.SetNodeRuleEnabled(ctx, "node-1", corenotification.TriggerContainerExit, false, "tester")
	if err != nil {
		t.Fatal(err)
	}
	if rule.Enabled || rule.Source != corenotification.SourceNode {
		t.Errorf("rule = %+v, want disabled from node", rule)
	}

	got, _, _ := repo.GetState(ctx, corenotification.TriggerContainerExit, "node-1", "web")
	if got.State != corenotification.StateOK || got.Severity != "" || got.ConditionSince != nil {
		t.Errorf("disabled rule state = %+v, want silently ok", got)
	}
	if _, ok := got.Baseline["seen_up"]; ok || got.Baseline["other"] != "kept" {
		t.Errorf("baseline = %v, want seen_up dropped and the rest kept", got.Baseline)
	}
	for _, other := range []struct {
		trigger corenotification.Trigger
		node    string
		subject string
	}{{corenotification.TriggerDiskPct, "node-1", "/"}, {corenotification.TriggerContainerExit, "node-2", "web"}} {
		s, _, _ := repo.GetState(ctx, other.trigger, other.node, other.subject)
		if s.State != corenotification.StateFiring {
			t.Errorf("%s on %s = %s, want untouched", other.trigger, other.node, s.State)
		}
	}
	var deliveries int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries`).Scan(&deliveries)
	if deliveries != 0 {
		t.Errorf("%d deliveries, want none from a silent reset", deliveries)
	}
}

func TestClearingANodeRuleIsIdempotentAndKeepsOtherOverrides(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")

	// Clearing with nothing to clear.
	if _, err := repo.ClearNodeRuleEnabled(ctx, "node-1", corenotification.TriggerDiskPct, "tester"); err != nil {
		t.Fatalf("clear without an override: %v", err)
	}

	// An override carrying only enabled goes away entirely.
	if _, err := repo.SetNodeRuleEnabled(ctx, "node-1", corenotification.TriggerRAMPct, false, "tester"); err != nil {
		t.Fatal(err)
	}
	rule, err := repo.ClearNodeRuleEnabled(ctx, "node-1", corenotification.TriggerRAMPct, "tester")
	if err != nil || !rule.Enabled || rule.Source != corenotification.SourceGlobal {
		t.Fatalf("after clear: %+v %v, want enabled from global", rule, err)
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_rules WHERE node_id = 'node-1' AND trigger = 'ram_pct'`).Scan(&rows)
	if rows != 0 {
		t.Errorf("%d ram_pct override rows, want the empty override deleted", rows)
	}

	// An override that also changes for_seconds keeps its row.
	forSeconds := 60
	if err := repo.SetRule(ctx, corenotification.RuleSpec{
		Trigger: corenotification.TriggerDiskPct, NodeID: "node-1", ForSeconds: &forSeconds,
	}, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetNodeRuleEnabled(ctx, "node-1", corenotification.TriggerDiskPct, false, "tester"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ClearNodeRuleEnabled(ctx, "node-1", corenotification.TriggerDiskPct, "tester"); err != nil {
		t.Fatal(err)
	}
	effective, err := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "node-1")
	if err != nil || !effective.Enabled || effective.ForSeconds != 60 {
		t.Errorf("effective = %+v %v, want enabled again with for_seconds 60 kept", effective, err)
	}
}

func TestGloballyDisabledRuleCanBeEnabledForOneNode(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	if _, err := pool.Exec(ctx, `UPDATE notification_rules SET enabled = false WHERE trigger = 'disk_pct' AND node_id IS NULL`); err != nil {
		t.Fatal(err)
	}
	rule, err := repo.SetNodeRuleEnabled(ctx, "node-1", corenotification.TriggerDiskPct, true, "tester")
	if err != nil || !rule.Enabled || rule.Source != corenotification.SourceNode || rule.GlobalEnabled == nil || *rule.GlobalEnabled {
		t.Fatalf("rule = %+v %v, want enabled on the node over a disabled global", rule, err)
	}
	n := notificationFor("on-for-one-node")
	c, _ := repo.CreateChannel(ctx, channelInput("c", nil))
	if got, err := repo.Enqueue(ctx, n, []corenotification.Channel{c}); err != nil || got != 1 {
		t.Errorf("enqueue = %d %v, want the node's enabled rule to enqueue", got, err)
	}
}

func TestNodeRuleErrors(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	for name, err := range map[string]error{
		"unknown node list": func() error { _, err := repo.ListNodeRules(ctx, "missing"); return err }(),
		"unknown node set": func() error {
			_, err := repo.SetNodeRuleEnabled(ctx, "missing", corenotification.TriggerDiskPct, false, "t")
			return err
		}(),
		"unknown rule": func() error {
			_, err := repo.SetNodeRuleEnabled(ctx, "node-1", "cpu_pct", false, "t")
			return err
		}(),
	} {
		if errs.CodeOf(err) != errs.CodeNotFound {
			t.Errorf("%s: %v, want not found", name, err)
		}
	}
}

func TestEnqueueSkipsARuleDisabledForTheNode(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	c, _ := repo.CreateChannel(ctx, channelInput("c", nil))
	if _, err := repo.SetNodeRuleEnabled(ctx, "node-1", corenotification.TriggerDiskPct, false, "t"); err != nil {
		t.Fatal(err)
	}
	if got, err := repo.Enqueue(ctx, notificationFor("disabled"), []corenotification.Channel{c}); err != nil || got != 0 {
		t.Errorf("enqueue for a disabled rule = %d %v, want 0", got, err)
	}
	test := corenotification.Notification{EventID: "test-1", Kind: corenotification.KindTest, Title: "t", Message: "m"}
	if got, err := repo.Enqueue(ctx, test, []corenotification.Channel{c}); err != nil || got != 1 {
		t.Errorf("test enqueue = %d %v, want 1", got, err)
	}
}

// An enqueue racing a rule change waits for it and then reads the new value,
// so nothing is enqueued for a rule once its disabling has committed.
func TestEnqueueWaitsForAConcurrentRuleChange(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	c, _ := repo.CreateChannel(ctx, channelInput("c", nil))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('disk_pct', 0))`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO notification_rules (id, trigger, node_id, enabled) VALUES ('o1', 'disk_pct', 'node-1', false)`); err != nil {
		t.Fatal(err)
	}

	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := repo.Enqueue(ctx, notificationFor("racing"), []corenotification.Channel{c})
		done <- result{n, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("enqueue finished while the rule change held its lock: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil || r.n != 0 {
			t.Errorf("enqueue after the disable committed = %d %v, want 0", r.n, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("enqueue never finished")
	}
}
