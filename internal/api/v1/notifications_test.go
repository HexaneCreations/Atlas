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
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
)

// A channel secret must never reach the API response: the only
// secret-named field allowed is the secret_configured flag. The same
// guarantee TestContainerDetailHasNoEnvironmentField pins for environment
// variables.
func TestNotificationChannelResponseHasNoSecretField(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(v1.NotificationChannelResponse{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if !strings.Contains(strings.ToLower(f.Name), "secret") {
			continue
		}
		if f.Name != "SecretConfigured" || f.Type.Kind() != reflect.Bool {
			t.Fatalf("NotificationChannelResponse exposes %q (%s); a channel secret must never be served", f.Name, f.Type)
		}
	}
}

// fakeNotificationStore is an in-memory [v1.NotificationStore]. It keeps
// secrets beside the channels, the way the real store keeps ciphertext, so
// tests can prove the secret reached the store yet never left it.
type fakeNotificationStore struct {
	channels   map[string]notification.Channel
	secrets    map[string]string
	deliveries []notification.Delivery
}

func newFakeNotificationStore() *fakeNotificationStore {
	return &fakeNotificationStore{channels: map[string]notification.Channel{}, secrets: map[string]string{}}
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
func (f *fakeNotificationStore) ChannelSecret(_ context.Context, id string) (string, error) {
	secret, ok := f.secrets[id]
	if !ok {
		return "", errs.New(errs.CodeFailedPrecondition, "no secret")
	}
	return secret, nil
}
func (f *fakeNotificationStore) put(id string, in notification.ChannelInput) notification.Channel {
	if in.Secret != nil {
		f.secrets[id] = *in.Secret
	}
	_, hasSecret := f.secrets[id]
	c := notification.Channel{
		ID: id, Name: in.Name, Type: in.Type, Enabled: in.Enabled, MinSeverity: in.MinSeverity,
		Config: in.Config, SecretConfigured: hasSecret,
	}
	f.channels[id] = c
	return c
}
func (f *fakeNotificationStore) CreateChannel(_ context.Context, in notification.ChannelInput) (notification.Channel, error) {
	return f.put("chan-1", in), nil
}
func (f *fakeNotificationStore) UpdateChannel(_ context.Context, id string, in notification.ChannelInput) (notification.Channel, error) {
	if _, ok := f.channels[id]; !ok {
		return notification.Channel{}, notFoundErr{}
	}
	return f.put(id, in), nil
}
func (f *fakeNotificationStore) DeleteChannel(_ context.Context, id string) error {
	delete(f.channels, id)
	delete(f.secrets, id)
	return nil
}
func (f *fakeNotificationStore) ListDeliveries(context.Context, notification.DeliveryFilter) (notification.DeliveryPage, error) {
	return notification.DeliveryPage{Deliveries: f.deliveries}, nil
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
	srv := httptest.NewServer(signedIn(handler))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateNotificationChannelAcceptsSecretButNeverReturnsIt(t *testing.T) {
	t.Parallel()
	store := newFakeNotificationStore()
	srv := newNotificationServer(t, store)

	const secret = `{"url":"https://prod-1.westus.logic.azure.com/workflows/run?sig=super-secret-signature"}`
	body := `{"name":"ops","type":"teams","secret":` + string(mustJSON(t, secret)) + `}`
	resp, err := http.Post(srv.URL+"/api/v1/notifications/channels", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	raw := mustReadAll(t, resp)
	if strings.Contains(raw, "super-secret-signature") {
		t.Fatalf("response leaked the secret: %s", raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded["secret_configured"] != true {
		t.Errorf("secret_configured = %v, want true", decoded["secret_configured"])
	}
	if decoded["min_severity"] != "warning" {
		t.Errorf("min_severity = %v, want the warning default", decoded["min_severity"])
	}
	if store.secrets["chan-1"] != secret {
		t.Fatal("the secret did not reach the store")
	}
}

func TestListNotificationChannelsOmitsSecrets(t *testing.T) {
	t.Parallel()
	store := newFakeNotificationStore()
	secret := "smtp-app-password"
	store.put("chan-1", notification.ChannelInput{
		Name: "ops", Type: notification.ChannelEmail, Enabled: true, MinSeverity: notification.SeverityCritical,
		Config: map[string]any{"host": "smtp.example.invalid"}, Secret: &secret,
	})
	srv := newNotificationServer(t, store)

	resp, err := http.Get(srv.URL + "/api/v1/notifications/channels")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	body := mustReadAll(t, resp)
	if strings.Contains(body, secret) {
		t.Fatalf("response leaked the secret: %s", body)
	}
	if !strings.Contains(body, `"secret_configured":true`) || !strings.Contains(body, `"host":"smtp.example.invalid"`) {
		t.Errorf("response is missing the flag or the config: %s", body)
	}
}

func TestDeleteNotificationChannelReturnsNoContent(t *testing.T) {
	t.Parallel()
	store := newFakeNotificationStore()
	store.channels["chan-1"] = notification.Channel{ID: "chan-1", Name: "ops", Type: notification.ChannelTeams}
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
