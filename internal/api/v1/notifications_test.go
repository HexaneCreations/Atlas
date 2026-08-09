package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
)

// A webhook signing secret must never reach the API response. This is the
// same guarantee TestContainerDetailHasNoEnvironmentField pins for
// environment variables.
func TestNotificationChannelResponseHasNoSecretField(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(v1.NotificationChannelResponse{})
	for i := range typ.NumField() {
		name := strings.ToLower(typ.Field(i).Name)
		if strings.Contains(name, "secret") {
			t.Fatalf("NotificationChannelResponse exposes %q; a webhook secret must never be served", typ.Field(i).Name)
		}
	}
}

type fakeNotificationStore struct {
	channels   map[string]notification.Channel
	deliveries []notification.Delivery
}

func newFakeNotificationStore() *fakeNotificationStore {
	return &fakeNotificationStore{channels: map[string]notification.Channel{}}
}

func (f *fakeNotificationStore) ListChannels(context.Context) ([]notification.Channel, error) {
	out := make([]notification.Channel, 0, len(f.channels))
	for _, c := range f.channels {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeNotificationStore) GetChannel(_ context.Context, id string) (notification.Channel, error) {
	c, ok := f.channels[id]
	if !ok {
		return notification.Channel{}, notFoundErr{}
	}
	return c, nil
}
func (f *fakeNotificationStore) CreateChannel(_ context.Context, c notification.Channel) (notification.Channel, error) {
	c.ID = "chan-1"
	f.channels[c.ID] = c
	return c, nil
}
func (f *fakeNotificationStore) UpdateChannel(_ context.Context, c notification.Channel) (notification.Channel, error) {
	f.channels[c.ID] = c
	return c, nil
}
func (f *fakeNotificationStore) DeleteChannel(_ context.Context, id string) error {
	delete(f.channels, id)
	return nil
}
func (f *fakeNotificationStore) ListDeliveries(context.Context, notification.DeliveryFilter) ([]notification.Delivery, error) {
	return f.deliveries, nil
}

type notFoundErr struct{}

func (notFoundErr) Error() string { return "not found" }

func newNotificationServer(t *testing.T, store *fakeNotificationStore) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config: &cfg, Health: health.NewRegistry(nil), Pool: postgres.NewPool(cfg.Database, nil),
		Bus: bus, NotificationStore: store,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateNotificationChannelAcceptsSecretButNeverReturnsIt(t *testing.T) {
	t.Parallel()
	store := newFakeNotificationStore()
	srv := newNotificationServer(t, store)

	body := `{"name":"ops","type":"webhook","webhook_url":"https://example.invalid/hook","webhook_secret":"shh"}`
	resp, err := http.Post(srv.URL+"/api/v1/notifications/channels", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k := range raw {
		if strings.Contains(strings.ToLower(k), "secret") {
			t.Fatalf("response contains a secret-named field %q: %v", k, raw)
		}
	}

	// But the store did receive the secret — it isn't silently dropped.
	stored, ok := store.channels["chan-1"]
	if !ok || stored.Webhook.Secret != "shh" {
		t.Fatalf("expected the secret to reach the store, got %+v", stored)
	}
}

func TestListNotificationChannelsOmitsSecrets(t *testing.T) {
	t.Parallel()
	store := newFakeNotificationStore()
	store.channels["chan-1"] = notification.Channel{
		ID: "chan-1", Name: "ops", Type: notification.ChannelWebhook, Enabled: true,
		Webhook: notification.WebhookConfig{URL: "https://example.invalid/hook", Secret: "shh"},
	}
	srv := newNotificationServer(t, store)

	resp, err := http.Get(srv.URL + "/api/v1/notifications/channels")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	body := mustReadAll(t, resp)
	if strings.Contains(body, "shh") {
		t.Fatalf("response leaked the webhook secret: %s", body)
	}
}

func TestDeleteNotificationChannelReturnsNoContent(t *testing.T) {
	t.Parallel()
	store := newFakeNotificationStore()
	store.channels["chan-1"] = notification.Channel{ID: "chan-1", Name: "ops", Type: notification.ChannelWebhook}
	srv := newNotificationServer(t, store)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/notifications/channels/chan-1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if _, ok := store.channels["chan-1"]; ok {
		t.Fatal("expected the channel removed from the store")
	}
}

func mustReadAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
