//go:build integration

package monitor_test

import (
	"context"
	"slices"
	"testing"
	"time"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/storage/monitor"
	"github.com/jackc/pgx/v5/pgxpool"
)

func seedChannel(t *testing.T, pool *pgxpool.Pool, id string, monitorDefault bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO notification_channels (id, name, type, monitor_default)
		VALUES ($1, $1, 'teams', $2)`, id, monitorDefault); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorChannelSelection(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	seedChannel(t, pool, "ch-default", true)
	seedChannel(t, pool, "ch-other", false)

	v, err := repo.CreateMonitor(ctx, newMonitor("Shop"), coremonitor.Secret{}, "alice")
	if err != nil || !slices.Equal(v.ChannelIDs, []string{"ch-default"}) {
		t.Fatalf("a new monitor's channels = %v, %v; want the monitor_default ones", v.ChannelIDs, err)
	}
	none := newMonitor("Quiet")
	none.ChannelIDs = []string{}
	q, err := repo.CreateMonitor(ctx, none, coremonitor.Secret{}, "alice")
	if err != nil || len(q.ChannelIDs) != 0 {
		t.Errorf("an explicit empty selection = %v, %v", q.ChannelIDs, err)
	}

	got, err := repo.SetMonitorChannels(ctx, v.ID, []string{"ch-other", "ch-default", "ch-other"})
	if err != nil || !slices.Equal(got, []string{"ch-default", "ch-other"}) {
		t.Errorf("SetMonitorChannels = %v, %v", got, err)
	}
	updated, err := repo.UpdateMonitor(ctx, v.ID, func(m coremonitor.Monitor, s coremonitor.Secret) (coremonitor.Monitor, coremonitor.Secret, error) {
		m.Name = "Shop 2"
		return m, s, nil
	}, "bob")
	if err != nil || len(updated.ChannelIDs) != 2 {
		t.Errorf("an update without a selection changed it: %v, %v", updated.ChannelIDs, err)
	}
	if _, err = repo.SetMonitorChannels(ctx, v.ID, []string{"missing"}); code(err) != "invalid_argument" {
		t.Errorf("an unknown channel: %v", err)
	}
	if ids, _ := repo.MonitorChannelIDs(ctx, v.ID); len(ids) != 2 {
		t.Errorf("a refused change altered the selection: %v", ids)
	}
	if _, err = repo.SetMonitorChannels(ctx, "nope", nil); code(err) != "not_found" {
		t.Errorf("unknown monitor: %v", err)
	}
	if got, setErr := repo.SetMonitorChannels(ctx, v.ID, nil); setErr != nil || len(got) != 0 {
		t.Errorf("clearing = %v, %v", got, setErr)
	}

	// Deleting a channel drops it from every selection.
	if _, err = repo.SetMonitorChannels(ctx, v.ID, []string{"ch-other"}); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = 'ch-other'`); err != nil {
		t.Fatal(err)
	}
	if ids, _ := repo.MonitorChannelIDs(ctx, v.ID); len(ids) != 0 {
		t.Errorf("a deleted channel is still selected: %v", ids)
	}

	name, host, err := repo.MonitorInfo(ctx, v.ID)
	if err != nil || name != "Shop 2" || host != "shop.example.com" {
		t.Errorf("MonitorInfo = %q %q %v; want the name and the host only", name, host, err)
	}
}

func TestCertEventsAndEvalMonitors(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	v, err := repo.CreateMonitor(ctx, newMonitor("Cert"), coremonitor.Secret{}, "alice")
	if err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(5 * 24 * time.Hour).Truncate(time.Second)
	makeDue(t, pool)
	due, err := repo.ClaimDue(ctx, 1, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("claim: %v %v", due, err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	rec := coremonitor.CheckRecord{MonitorID: v.ID, At: now, Counters: coremonitor.Counters{Status: coremonitor.StatusUp, StatusSince: now},
		Heartbeat: coremonitor.StatusUp, Result: coremonitor.Result{Up: true, StatusCode: 200, ResponseMS: 40, CertExpiresAt: &expires},
		NextCheckAt: now.Add(time.Minute), CertExpiresAt: &expires,
		CertEvent: coremonitor.CertEvent(due[0].Monitor, due[0].Counters, &expires, now)}
	if rec.CertEvent == nil || rec.CertEvent.Kind != coremonitor.EventCertWarn {
		t.Fatalf("first certificate 5 days from expiry: %+v", rec.CertEvent)
	}
	if err = repo.RecordCheck(ctx, rec); err != nil {
		t.Fatal(err)
	}
	events, err := repo.Events(ctx, v.ID, time.Now().Add(time.Minute), 10)
	if err != nil || len(events) != 1 || events[0].Kind != coremonitor.EventCertWarn || events[0].CertExpiresAt == nil ||
		!events[0].CertExpiresAt.Equal(expires) {
		t.Errorf("events = %+v, %v", events, err)
	}

	// The next claim carries what this check saw.
	makeDue(t, pool)
	due, err = repo.ClaimDue(ctx, 1, time.Minute)
	if err != nil || len(due) != 1 || due[0].Counters.CertExpiresAt == nil || due[0].Counters.LastCheckAt == nil {
		t.Fatalf("second claim counters = %+v, %v", due, err)
	}
	if e := coremonitor.CertEvent(due[0].Monitor, due[0].Counters, &expires, time.Now()); e != nil {
		t.Errorf("an unchanged certificate recorded %+v", e)
	}

	list, err := repo.ListEvalMonitors(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListEvalMonitors = %v, %v", list, err)
	}
	m := list[0]
	if m.MonitorID != v.ID || !m.Active || m.Status != "up" || m.CertExpiresAt == nil || !m.CertExpiresAt.Equal(expires) ||
		m.CertWarnDays != 14 || m.CertCritDays != 3 || !m.CertNotify || m.LastStatusCode == nil || *m.LastStatusCode != 200 {
		t.Errorf("eval monitor = %+v", m)
	}
}
