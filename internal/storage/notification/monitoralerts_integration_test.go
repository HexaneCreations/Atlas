//go:build integration

package notification_test

import (
	"context"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedMonitor(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	for _, q := range []string{
		`INSERT INTO monitors (id, name, url) VALUES ($1, $1, 'https://example.com/')`,
		`INSERT INTO monitor_status (monitor_id, next_check_at) VALUES ($1, now())`,
	} {
		if _, err := pool.Exec(context.Background(), q, id); err != nil {
			t.Fatal(err)
		}
	}
}

func firingMonitorState(monitorID string, trigger corenotification.Trigger) corenotification.State {
	now := time.Now()
	return corenotification.State{Trigger: trigger, MonitorID: monitorID, State: corenotification.StateFiring,
		Severity: corenotification.SeverityCritical, ConditionSince: &now, StateSince: now,
		Baseline: map[string]any{"down_since": now.UTC().Format(time.RFC3339Nano)}}
}

func TestMonitorStatesLiveBesideNodeStates(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")
	seedMonitor(t, pool, "mon-1")
	seedMonitor(t, pool, "mon-2")
	for _, s := range []corenotification.State{
		firingState("node-1", corenotification.TriggerNodeDown, ""),
		firingMonitorState("mon-1", corenotification.TriggerWebsiteDown),
		firingMonitorState("mon-2", corenotification.TriggerWebsiteDown),
	} {
		if err := repo.UpsertState(ctx, s); err != nil {
			t.Fatalf("UpsertState(%+v): %v", s, err)
		}
	}
	// An upsert of an existing monitor row replaces it.
	again := firingMonitorState("mon-1", corenotification.TriggerWebsiteDown)
	again.FlapCount = 4
	if err := repo.UpsertState(ctx, again); err != nil {
		t.Fatal(err)
	}
	var total int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_state`).Scan(&total)
	if total != 3 {
		t.Errorf("rows = %d, want 3", total)
	}
	got, found, err := repo.GetMonitorState(ctx, corenotification.TriggerWebsiteDown, "mon-1", "")
	if err != nil || !found || got.FlapCount != 4 || got.MonitorID != "mon-1" || got.NodeID != "" {
		t.Errorf("GetMonitorState = %+v, %v, %v", got, found, err)
	}
	nodes, _ := repo.ListActiveStates(ctx)
	monitors, _ := repo.ListActiveMonitorStates(ctx)
	if len(nodes) != 1 || nodes[0].NodeID != "node-1" || len(monitors) != 2 {
		t.Errorf("active node states %d, monitor states %d", len(nodes), len(monitors))
	}

	// BeginPending keeps a row that already left ok.
	pending := corenotification.State{Trigger: corenotification.TriggerWebsiteDown, MonitorID: "mon-2",
		State: corenotification.StatePending, StateSince: time.Now()}
	stored, err := repo.BeginPending(ctx, pending)
	if err != nil || stored.State != corenotification.StateFiring {
		t.Errorf("BeginPending over firing = %+v, %v", stored, err)
	}

	// Deleting a monitor takes its states and overrides with it.
	if _, err := repo.SetMonitorRuleEnabled(ctx, "mon-2", corenotification.TriggerWebsiteCertExpiry, false, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM monitors WHERE id = 'mon-2'`); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notification_state WHERE monitor_id = 'mon-2')
		+ (SELECT count(*) FROM notification_rules WHERE monitor_id = 'mon-2')`).Scan(&left)
	if left != 0 {
		t.Errorf("%d rows of a deleted monitor remain", left)
	}
}

func TestMonitorRuleDisableResetsStateAndBlocksEnqueue(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedMonitor(t, pool, "mon-1")
	seedMonitor(t, pool, "mon-2")
	for _, id := range []string{"mon-1", "mon-2"} {
		if err := repo.UpsertState(ctx, firingMonitorState(id, corenotification.TriggerWebsiteDown)); err != nil {
			t.Fatal(err)
		}
	}
	rule, err := repo.SetMonitorRuleEnabled(ctx, "mon-1", corenotification.TriggerWebsiteDown, false, "alice")
	if err != nil || rule.Enabled || rule.Source != corenotification.SourceMonitor {
		t.Fatalf("SetMonitorRuleEnabled = %+v, %v", rule, err)
	}
	if s, _, _ := repo.GetMonitorState(ctx, corenotification.TriggerWebsiteDown, "mon-1", ""); s.State != corenotification.StateOK {
		t.Errorf("mon-1 state = %s, want reset in the same write", s.State)
	}
	if s, _, _ := repo.GetMonitorState(ctx, corenotification.TriggerWebsiteDown, "mon-2", ""); s.State != corenotification.StateFiring {
		t.Errorf("mon-2 state = %s, want untouched", s.State)
	}

	channel, err := repo.CreateChannel(ctx, channelInput("ops", nil))
	if err != nil {
		t.Fatal(err)
	}
	n := corenotification.Notification{EventID: "e1", Kind: corenotification.KindFiring, Trigger: corenotification.TriggerWebsiteDown,
		MonitorID: "mon-1", Severity: corenotification.SeverityCritical, Title: "t", Message: "m",
		Details: corenotification.MonitorDetails{Failure: "timeout"}}
	if got, enqErr := repo.Enqueue(ctx, n, []corenotification.Channel{channel}); enqErr != nil || got != 0 {
		t.Errorf("Enqueue for a disabled monitor rule = %d, %v; want skipped", got, enqErr)
	}
	n.EventID, n.MonitorID = "e2", "mon-2"
	if got, enqErr := repo.Enqueue(ctx, n, []corenotification.Channel{channel}); enqErr != nil || got != 1 {
		t.Fatalf("Enqueue for mon-2 = %d, %v", got, enqErr)
	}
	page, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{})
	if err != nil || len(page.Deliveries) != 1 || page.Deliveries[0].MonitorID != "mon-2" || page.Deliveries[0].Details.Failure != "timeout" {
		t.Errorf("deliveries = %+v, %v", page.Deliveries, err)
	}

	if _, err = repo.ClearMonitorRuleEnabled(ctx, "mon-1", corenotification.TriggerWebsiteDown, "alice"); err != nil {
		t.Fatal(err)
	}
	var overrides int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_rules WHERE monitor_id IS NOT NULL`).Scan(&overrides)
	if overrides != 0 {
		t.Errorf("a cleared override left %d rows", overrides)
	}
	// Node endpoints and node resolution never see monitor rows.
	nodeRules, err := repo.ListNodeRules(ctx, mustNode(t, pool))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range nodeRules {
		if r.Kind != corenotification.SubjectNode {
			t.Errorf("node rules list %s", r.Key)
		}
	}

	for _, c := range []struct {
		name string
		err  error
	}{
		{"unknown monitor", func() error {
			_, err := repo.SetMonitorRuleEnabled(ctx, "nope", corenotification.TriggerWebsiteDown, false, "a")
			return err
		}()},
		{"node rule on a monitor", func() error {
			_, err := repo.SetMonitorRuleEnabled(ctx, "mon-1", corenotification.TriggerDiskPct, false, "a")
			return err
		}()},
		{"monitor rule on a node", func() error {
			_, err := repo.SetNodeRuleEnabled(ctx, "node-x", corenotification.TriggerWebsiteDown, false, "a")
			return err
		}()},
	} {
		if errs.CodeOf(c.err) != errs.CodeNotFound {
			t.Errorf("%s: %v, want not found", c.name, c.err)
		}
	}
}

func mustNode(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	seedNode(t, pool, "node-r")
	return "node-r"
}

func TestMonitorMuteEndsWithASilentReset(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedMonitor(t, pool, "mon-1")
	if err := repo.UpsertState(ctx, firingMonitorState("mon-1", corenotification.TriggerWebsiteDown)); err != nil {
		t.Fatal(err)
	}
	until, err := repo.MuteMonitor(ctx, "mon-1", corenotification.MutedIndefinitely)
	if err != nil || until == nil || !until.Equal(corenotification.MutedIndefinitely) {
		t.Fatalf("MuteMonitor = %v, %v", until, err)
	}
	if did, err := repo.EndMonitorMuteAfterExpiry(ctx, "mon-1", time.Now()); err != nil || did {
		t.Errorf("an unexpired mute ended: %v, %v", did, err)
	}
	if ok, err := repo.EndMonitorMuteExplicitly(ctx, "mon-1"); err != nil || !ok {
		t.Fatalf("EndMonitorMuteExplicitly = %v, %v", ok, err)
	}
	s, _, _ := repo.GetMonitorState(ctx, corenotification.TriggerWebsiteDown, "mon-1", "")
	var muted *time.Time
	_ = pool.QueryRow(ctx, `SELECT alerts_muted_until FROM monitors WHERE id = 'mon-1'`).Scan(&muted)
	if s.State != corenotification.StateOK || muted != nil {
		t.Errorf("after unmute: state %s, muted %v", s.State, muted)
	}
	if _, err := repo.MuteMonitor(ctx, "nope", time.Now()); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("muting an unknown monitor: %v", err)
	}
	if ok, err := repo.EndMonitorMuteExplicitly(ctx, "nope"); err != nil || ok {
		t.Errorf("unmuting an unknown monitor = %v, %v", ok, err)
	}
}

func TestChannelMonitorDefaultRoundTrips(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	in := channelInput("ops", nil)
	in.MonitorDefault = ptr(true)
	c, err := repo.CreateChannel(ctx, in)
	if err != nil || !c.MonitorDefault {
		t.Fatalf("create = %+v, %v", c, err)
	}
	in.MonitorDefault = nil
	if c, err = repo.UpdateChannel(ctx, c.ID, in); err != nil || !c.MonitorDefault {
		t.Errorf("an update without monitor_default changed it: %+v, %v", c, err)
	}
	in.MonitorDefault = ptr(false)
	if c, err = repo.UpdateChannel(ctx, c.ID, in); err != nil || c.MonitorDefault {
		t.Errorf("update to false = %+v, %v", c, err)
	}
}
