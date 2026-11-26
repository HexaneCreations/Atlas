package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/log"
)

const secretPath = "T0SECRETPATH"

// recordingSender is a [notification.Sender] that never touches the network.
type recordingSender struct {
	mu      sync.Mutex
	err     error
	secrets []string
}

func (s *recordingSender) Send(_ context.Context, _ notification.Channel, secret string, _ notification.Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secrets = append(s.secrets, secret)
	return s.err
}

func (s *recordingSender) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.secrets)
}

// engineStore adds the two halves of [notification.Engine]'s stores the API
// never sees to a fakeNotificationStore.
type engineStore struct {
	*fakeNotificationStore
	mu sync.Mutex
}

func (s *engineStore) Enqueue(_ context.Context, n notification.Notification, channels []notification.Channel) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range channels {
		s.deliveries = append(s.deliveries, notification.Delivery{
			ID: n.EventID, EventID: n.EventID, ChannelID: c.ID, Kind: n.Kind, Status: notification.StatusPending,
		})
	}
	return len(channels), nil
}

func (s *engineStore) ClaimByEvent(_ context.Context, eventID, channelID string, _ time.Time) (notification.Delivery, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deliveries {
		if d.EventID == eventID && d.ChannelID == channelID {
			return d, true, nil
		}
	}
	return notification.Delivery{}, false, nil
}

func (s *engineStore) finish(id string, status notification.Status, lastErr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.deliveries {
		if s.deliveries[i].ID == id {
			s.deliveries[i].Status, s.deliveries[i].LastError = status, lastErr
		}
	}
	return nil
}

func (s *engineStore) MarkDelivered(_ context.Context, id string, _ time.Time) error {
	return s.finish(id, notification.StatusDelivered, "")
}
func (s *engineStore) MarkExhausted(_ context.Context, id string, _ int, lastErr string) error {
	return s.finish(id, notification.StatusFailed, lastErr)
}
func (s *engineStore) MarkRetry(context.Context, string, int, time.Time, string) error { return nil }
func (s *engineStore) ClaimDue(context.Context, time.Time, time.Time, int) ([]notification.Delivery, error) {
	return nil, nil
}
func (s *engineStore) SweepDeliveries(context.Context, time.Time) (notification.SweepResult, error) {
	return notification.SweepResult{}, nil
}

// newFakeTester is the real engine over a fake store and sender.
func newFakeTester(store *fakeNotificationStore, sender notification.Sender) *notification.Engine {
	s := &engineStore{fakeNotificationStore: store}
	return notification.NewEngine(notification.Options{
		Channels: s, Deliveries: s,
		Senders: map[notification.ChannelType]notification.Sender{
			notification.ChannelTeams: sender, notification.ChannelWebhook: sender,
		},
	})
}

func webhookSecret(url string) string {
	return fmt.Sprintf(`{"url":%q,"headers":{"X-Api":"hdr-value-99"}}`, url)
}

func draftBody(url string) string {
	raw, _ := json.Marshal(map[string]any{
		"name": "draft", "type": "webhook", "config": map[string]any{"template": "{{.msg}}"}, "secret": webhookSecret(url),
	})
	return string(raw)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testSendFixture struct {
	t      *testing.T
	store  *fakeNotificationStore
	sender *recordingSender
	url    string
	client *http.Client
	logs   *lockedBuffer
	now    time.Time
}

func newTestSendFixture(t *testing.T) *testSendFixture {
	t.Helper()
	f := &testSendFixture{t: t, store: newFakeNotificationStore(), sender: &recordingSender{}, logs: &lockedBuffer{}, now: time.Now()}
	secret := webhookSecret("https://hooks.example.com/services/" + secretPath)
	f.store.put("chan-1", notification.ChannelInput{
		Name: "ops", Type: notification.ChannelWebhook, MinSeverity: notification.SeverityCritical,
		Config: map[string]any{"template": "{{.msg}}"}, Secret: &secret,
	})

	u := testUser(t, "tester", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, roleAuthorizer{role: user.RoleSuperadmin}, f.store, func(d *api.Deps) {
		d.NotificationTester = newFakeTester(f.store, f.sender)
		d.NotificationTestLimiter = notification.NewTestLimiter(func() time.Time { return f.now })
	})
	// Requests carry the server's own logger; capture what handlers write.
	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Config.Handler.ServeHTTP(w, r.WithContext(log.Into(r.Context(), logger)))
	}))
	t.Cleanup(front.Close)
	f.url = front.URL
	f.client = mustClientWithCookies(t)
	login(t, f.client, f.url, u.Username, "correct horse battery staple").Body.Close()
	return f
}

func (f *testSendFixture) post(path, body string) (*http.Response, string) {
	f.t.Helper()
	resp, err := f.client.Post(f.url+"/api/v1"+path, "application/json", strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp, mustReadAll(f.t, resp)
}

func (f *testSendFixture) wantOutcome(resp *http.Response, raw string, ok bool) string {
	f.t.Helper()
	var out struct {
		OK    *bool   `json:"ok"`
		Error *string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || resp.StatusCode != http.StatusOK || out.OK == nil || out.Error == nil {
		f.t.Fatalf("status %d body %s, want 200 {ok, error}", resp.StatusCode, raw)
	}
	if *out.OK != ok || (*out.Error == "") != ok {
		f.t.Fatalf("outcome %s, want ok=%v", raw, ok)
	}
	return *out.Error
}

// noLeak checks a response and everything logged so far.
func (f *testSendFixture) noLeak(raw string) {
	f.t.Helper()
	for _, s := range []string{secretPath, "hooks.example.com", "hdr-value-99", "X-Api"} {
		if strings.Contains(raw, s) {
			f.t.Errorf("response leaks %q: %s", s, raw)
		}
		if strings.Contains(f.logs.String(), s) {
			f.t.Errorf("log leaks %q: %s", s, f.logs.String())
		}
	}
}

func TestTestSavedChannel(t *testing.T) {
	f := newTestSendFixture(t) // chan-1 is disabled and critical-only: ignored by a test
	resp, raw := f.post("/notifications/channels/chan-1/test", "")
	f.wantOutcome(resp, raw, true)
	if len(f.store.deliveries) != 1 || f.store.deliveries[0].Kind != notification.KindTest || f.store.deliveries[0].Status != notification.StatusDelivered {
		t.Errorf("deliveries = %+v, want one delivered test", f.store.deliveries)
	}

	f.now = f.now.Add(time.Minute)
	f.sender.err = errors.New("500 from https://hooks.example.com/services/" + secretPath + " with /services/" + secretPath + " X-Api: hdr-value-99")
	resp, raw = f.post("/notifications/channels/chan-1/test", "")
	f.wantOutcome(resp, raw, false)
	f.noLeak(raw)
	if d := f.store.deliveries[1]; d.Status != notification.StatusFailed || strings.Contains(d.LastError, secretPath) {
		t.Errorf("failed delivery = %+v", d)
	}
	if !strings.Contains(f.logs.String(), `"msg":"notification channel tested"`) || !strings.Contains(f.logs.String(), `"actor_user_id"`) {
		t.Errorf("no admin log line for the test: %s", f.logs.String())
	}
}

func TestTestDraftChannel(t *testing.T) {
	f := newTestSendFixture(t)
	resp, raw := f.post("/notifications/channels/test", draftBody("https://hooks.example.com/services/"+secretPath))
	f.wantOutcome(resp, raw, true)
	f.noLeak(raw)

	f.now = f.now.Add(time.Minute)
	f.sender.err = errors.New("no route to /services/" + secretPath)
	resp, raw = f.post("/notifications/channels/test", draftBody("https://hooks.example.com/services/"+secretPath))
	f.wantOutcome(resp, raw, false)
	f.noLeak(raw)

	if len(f.store.deliveries) != 0 || len(f.store.channels) != 1 {
		t.Errorf("a draft test stored something: %+v %+v", f.store.deliveries, f.store.channels)
	}
}

func TestTestDraftWithTheStoredSecret(t *testing.T) {
	f := newTestSendFixture(t)
	body := `{"name":"renamed","type":"webhook","config":{"template":"{{.msg}}"},"secret":"","channel_id":"chan-1"}`
	resp, raw := f.post("/notifications/channels/test", body)
	f.wantOutcome(resp, raw, true)
	f.noLeak(raw)
	if f.sender.calls() != 1 || f.sender.secrets[0] != f.store.secrets["chan-1"] {
		t.Error("the stored secret was not used")
	}
	if resp, raw := f.post("/notifications/channels/test", strings.Replace(body, "webhook", "teams", 1)); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("another type: %d %s, want 400", resp.StatusCode, raw)
	}
}

func TestTestDraftRejectsInvalidInputWithoutSending(t *testing.T) {
	f := newTestSendFixture(t)
	for name, body := range map[string]string{
		"loopback":      draftBody("https://127.0.0.1/services/" + secretPath),
		"private":       draftBody("https://10.1.2.3/services/" + secretPath),
		"metadata":      draftBody("https://169.254.169.254/latest/" + secretPath),
		"localhost":     draftBody("https://localhost/" + secretPath),
		"http":          draftBody("http://hooks.example.com/" + secretPath),
		"teams host":    `{"name":"d","type":"teams","secret":"{\"url\":\"https://hooks.example.com/` + secretPath + `\"}"}`,
		"no secret":     `{"name":"d","type":"teams"}`,
		"unknown field": `{"name":"d","type":"teams","hooks.example.com":"` + secretPath + `"}`,
		"not json":      `https://hooks.example.com/` + secretPath,
		"empty":         ``,
		"too large":     `{"name":"` + strings.Repeat("a", 2<<20) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			resp, raw := f.post("/notifications/channels/test", body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status %d: %.200s, want 400", resp.StatusCode, raw)
			}
			f.noLeak(raw)
		})
	}
	if f.sender.calls() != 0 {
		t.Errorf("%d rejected drafts were sent", f.sender.calls())
	}
	// None of them used up the user's budget.
	resp, raw := f.post("/notifications/channels/test", draftBody("https://hooks.example.com/x"))
	f.wantOutcome(resp, raw, true)
}

func TestTestEndpointsAreRateLimited(t *testing.T) {
	f := newTestSendFixture(t)
	resp, raw := f.post("/notifications/channels/chan-1/test", "")
	f.wantOutcome(resp, raw, true)
	f.now = f.now.Add(1500 * time.Millisecond)
	for _, c := range []struct{ path, body string }{
		{"/notifications/channels/chan-1/test", ""},
		{"/notifications/channels/test", draftBody("https://other.example.net/x")},
	} {
		resp, raw := f.post(c.path, c.body)
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "4" {
			t.Errorf("%s: %d Retry-After %q: %s, want 429 and 4", c.path, resp.StatusCode, resp.Header.Get("Retry-After"), raw)
		}
	}
	if f.sender.calls() != 1 {
		t.Errorf("sent %d, want 1", f.sender.calls())
	}
	f.now = f.now.Add(4 * time.Second)
	resp, raw = f.post("/notifications/channels/chan-1/test", "")
	f.wantOutcome(resp, raw, true)
}

// A client that goes away mid-send does not cancel it: a cancelled send
// would leave its delivery pending, and the dispatcher would send it again.
func TestTestSendOutlivesTheRequest(t *testing.T) {
	store := newFakeNotificationStore()
	secret := webhookSecret("https://hooks.example.com/x")
	store.put("chan-1", notification.ChannelInput{Name: "ops", Type: notification.ChannelWebhook, Config: map[string]any{"template": "x"}, Secret: &secret})
	sender := &ctxSender{entered: make(chan struct{}), release: make(chan struct{}), result: make(chan error, 1)}
	srv := newNotificationServerWith(t, store, sender)

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v1/notifications/channels/chan-1/test", nil)
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-sender.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the test send never reached the sender")
	}
	cancel()
	time.Sleep(100 * time.Millisecond) // let the server notice
	close(sender.release)
	if err := <-sender.result; err != nil {
		t.Errorf("the send's context ended with the request: %v", err)
	}
}

type ctxSender struct {
	entered, release chan struct{}
	result           chan error
}

func (s *ctxSender) Send(ctx context.Context, _ notification.Channel, _ string, _ notification.Delivery) error {
	close(s.entered)
	<-s.release
	s.result <- ctx.Err()
	return nil
}

func newNotificationServerWith(t *testing.T, store *fakeNotificationStore, sender notification.Sender) *httptest.Server {
	t.Helper()
	u := testUser(t, "anyone", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, nil, store, func(d *api.Deps) {
		d.Authz = nil
		d.NotificationTester = newFakeTester(store, sender)
		d.Sessions, d.Users = nil, nil
	})
	// Every route needs a session; with no session store, supply one.
	front := httptest.NewServer(signedIn(srv.Config.Handler))
	t.Cleanup(front.Close)
	return front
}

func TestChannelChangesAreLoggedWithoutSecrets(t *testing.T) {
	f := newTestSendFixture(t)
	body := `{"name":"new","type":"webhook","config":{"template":"{{.msg}}"},"secret":` + string(mustJSON(t, webhookSecret("https://hooks.example.com/"+secretPath))) + `}`
	for _, a := range []guardAction{
		{"create", http.MethodPost, "/notifications/channels", body},
		{"update", http.MethodPut, "/notifications/channels/chan-1", body},
		{"delete", http.MethodDelete, "/notifications/channels/chan-1", ""},
	} {
		if code := do(t, f.client, f.url, a); code >= 300 {
			t.Fatalf("%s: %d", a.name, code)
		}
		if !strings.Contains(f.logs.String(), `"msg":"notification channel `+a.name+`d"`) {
			t.Errorf("%s was not logged", a.name)
		}
	}
	f.noLeak("")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
