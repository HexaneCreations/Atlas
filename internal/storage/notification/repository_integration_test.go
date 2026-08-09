//go:build integration

package notification_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/notification"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *notification.Repository {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}

	admin, err := pgxpool.NewWithConfig(context.Background(), parsed)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("atlas_notification_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.NewWithConfig(context.Background(), parsed)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := config.Default()
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = name
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	cfg.Database.MigrateOnStart = true

	pool := postgres.NewPool(cfg.Database, log.Discard())
	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	migrator := postgres.NewMigrator(pool.DB(), migrations.FS, log.Discard())
	if _, err := migrator.Apply(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	return notification.NewRepository(pool.DB())
}

func testChannel() corenotification.Channel {
	return corenotification.Channel{
		Name: "ops-webhook", Type: corenotification.ChannelWebhook, Enabled: true,
		Webhook: corenotification.WebhookConfig{URL: "https://example.invalid/hook", Secret: "shh"},
	}
}

func TestCreateAndGetChannelRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" {
		t.Fatal("expected an assigned id")
	}

	got, err := repo.GetChannel(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "ops-webhook" || got.Webhook.URL != "https://example.invalid/hook" || got.Webhook.Secret != "shh" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if !got.Enabled {
		t.Error("expected enabled = true")
	}
}

func TestGetChannelNotFound(t *testing.T) {
	repo := newRepository(t)
	_, err := repo.GetChannel(context.Background(), "does-not-exist")
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("code = %q, want not_found", errs.CodeOf(err))
	}
}

func TestUpdateChannelPreservesCreatedAt(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	updated := created
	updated.Enabled = false
	updated.Webhook.URL = "https://example.invalid/other"
	result, err := repo.UpdateChannel(ctx, updated)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if result.Enabled || result.Webhook.URL != "https://example.invalid/other" {
		t.Fatalf("update did not apply: %+v", result)
	}
	if !result.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("created_at changed: got %v, want %v", result.CreatedAt, created.CreatedAt)
	}
}

func TestDeleteChannelRemovesIt(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	created, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.DeleteChannel(ctx, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.GetChannel(ctx, created.ID); errs.CodeOf(err) != errs.CodeNotFound {
		t.Fatalf("code after delete = %q, want not_found", errs.CodeOf(err))
	}
}

func TestEnqueueIsIdempotentPerEventAndChannel(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	channel, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}

	event := corenotification.Event{
		ID: "evt-dup", Trigger: corenotification.TriggerAlertTransition,
		NodeID: "node-1", Severity: "critical", Title: "High CPU", Time: time.Now(),
	}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{channel}); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{channel}); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}

	due, err := repo.DueDeliveries(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	count := 0
	for _, d := range due {
		if d.EventID == "evt-dup" && d.ChannelID == channel.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 delivery after two Enqueue calls, got %d", count)
	}
}

func TestEnqueueFansOutToEveryChannel(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	c1, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create c1: %v", err)
	}
	c2spec := testChannel()
	c2spec.Name = "second"
	c2, err := repo.CreateChannel(ctx, c2spec)
	if err != nil {
		t.Fatalf("create c2: %v", err)
	}

	event := corenotification.Event{ID: "evt-fanout", Trigger: corenotification.TriggerAlertTransition, Time: time.Now()}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{c1, c2}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	due, err := repo.DueDeliveries(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	channels := map[string]bool{}
	for _, d := range due {
		if d.EventID == "evt-fanout" {
			channels[d.ChannelID] = true
		}
	}
	if len(channels) != 2 {
		t.Fatalf("expected deliveries fanned out to 2 channels, got %d", len(channels))
	}
}

func TestDueDeliveriesExcludesFutureAttempts(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	channel, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	event := corenotification.Event{ID: "evt-future", Time: time.Now()}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{channel}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Not due yet: "now" is before the delivery's next_attempt_at (set at
	// enqueue time to "now"), so querying strictly in the past excludes it.
	due, err := repo.DueDeliveries(ctx, time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	for _, d := range due {
		if d.EventID == "evt-future" {
			t.Fatal("expected the delivery not to be due an hour before it was enqueued")
		}
	}
}

func TestMarkDeliveredRemovesFromDueDeliveries(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	channel, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	event := corenotification.Event{ID: "evt-delivered", Time: time.Now()}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{channel}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	due, err := repo.DueDeliveries(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	var id string
	for _, d := range due {
		if d.EventID == "evt-delivered" {
			id = d.ID
		}
	}
	if id == "" {
		t.Fatal("expected the delivery to be due")
	}

	if err := repo.MarkDelivered(ctx, id, time.Now()); err != nil {
		t.Fatalf("mark delivered: %v", err)
	}

	after, err := repo.DueDeliveries(ctx, time.Now().Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("due after: %v", err)
	}
	for _, d := range after {
		if d.ID == id {
			t.Fatal("delivered delivery still reported as due")
		}
	}

	list, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{Status: corenotification.StatusDelivered})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found bool
	for _, d := range list {
		if d.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("expected the delivered delivery in the delivered-status list")
	}
}

func TestMarkRetryKeepsItPendingAndReschedules(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	channel, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	event := corenotification.Event{ID: "evt-retry", Time: time.Now()}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{channel}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	due, _ := repo.DueDeliveries(ctx, time.Now().Add(time.Minute), 100)
	var id string
	for _, d := range due {
		if d.EventID == "evt-retry" {
			id = d.ID
		}
	}

	future := time.Now().Add(time.Hour)
	if err := repo.MarkRetry(ctx, id, 1, future, "connection refused"); err != nil {
		t.Fatalf("mark retry: %v", err)
	}

	// Not due immediately after a retry is scheduled an hour out.
	stillDue, _ := repo.DueDeliveries(ctx, time.Now(), 100)
	for _, d := range stillDue {
		if d.ID == id {
			t.Fatal("expected the retried delivery not due yet")
		}
	}

	// Due once "now" passes the rescheduled time.
	nowDue, err := repo.DueDeliveries(ctx, future.Add(time.Second), 100)
	if err != nil {
		t.Fatalf("due: %v", err)
	}
	var got corenotification.Delivery
	for _, d := range nowDue {
		if d.ID == id {
			got = d
		}
	}
	if got.ID == "" {
		t.Fatal("expected the delivery due once its rescheduled time passes")
	}
	if got.Attempts != 1 || got.LastError != "connection refused" {
		t.Errorf("delivery = %+v, want attempts=1 and the recorded error", got)
	}
	if got.Status != corenotification.StatusPending {
		t.Errorf("status = %v, want pending (a retry is not a terminal outcome)", got.Status)
	}
}

func TestMarkExhaustedRemovesFromDueDeliveries(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	channel, err := repo.CreateChannel(ctx, testChannel())
	if err != nil {
		t.Fatalf("create channel: %v", err)
	}
	event := corenotification.Event{ID: "evt-exhausted", Time: time.Now()}
	if err := repo.Enqueue(ctx, event, []corenotification.Channel{channel}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	due, _ := repo.DueDeliveries(ctx, time.Now().Add(time.Minute), 100)
	var id string
	for _, d := range due {
		if d.EventID == "evt-exhausted" {
			id = d.ID
		}
	}

	if err := repo.MarkExhausted(ctx, id, 5, "gave up"); err != nil {
		t.Fatalf("mark exhausted: %v", err)
	}

	after, _ := repo.DueDeliveries(ctx, time.Now().Add(24*time.Hour), 100)
	for _, d := range after {
		if d.ID == id {
			t.Fatal("exhausted delivery still reported as due")
		}
	}

	failed, err := repo.ListDeliveries(ctx, corenotification.DeliveryFilter{ChannelID: channel.ID, Status: corenotification.StatusFailed})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var found corenotification.Delivery
	for _, d := range failed {
		if d.ID == id {
			found = d
		}
	}
	if found.ID == "" {
		t.Fatal("expected the exhausted delivery in the failed-status list")
	}
	if found.Attempts != 5 || found.LastError != "gave up" {
		t.Errorf("delivery = %+v, want attempts=5 and the recorded error", found)
	}
}
