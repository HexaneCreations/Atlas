//go:build integration

package app_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hexane/atlas/internal/platform/config"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes a running
// server's logger makes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func debugLogger(buf *syncBuffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

var notificationEndpoints = []struct{ method, path, body string }{
	{http.MethodGet, "/api/v1/notifications/channels", ""},
	{http.MethodPost, "/api/v1/notifications/channels", `{"name":"it-nokey","type":"teams"}`},
	{http.MethodGet, "/api/v1/notifications/channels/missing", ""},
	{http.MethodPut, "/api/v1/notifications/channels/missing", `{"name":"it-nokey","type":"teams"}`},
	{http.MethodDelete, "/api/v1/notifications/channels/missing", ""},
	{http.MethodGet, "/api/v1/notifications/deliveries", ""},
}

// Without an encryption key the server still starts, warns exactly once,
// never starts the dispatcher, and answers 503 on every channel endpoint.
func TestNotificationsAreDisabledWithoutAnEncryptionKey(t *testing.T) {
	var logs syncBuffer
	base := bootServerWith(t, func(c *config.Config) { c.Notifications.EncryptionKey = "" }, debugLogger(&logs))
	client := authenticatedTestClient(t, base, "superadmin")

	for _, e := range notificationEndpoints {
		req, err := http.NewRequest(e.method, base+e.path, strings.NewReader(e.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", e.method, e.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s status = %d, want 503", e.method, e.path, resp.StatusCode)
		}
	}

	out := logs.String()
	if n := strings.Count(out, "notifications are disabled: ATLAS_NOTIFICATIONS_ENCRYPTION_KEY is not set"); n != 1 {
		t.Errorf("disabled warning logged %d times, want 1", n)
	}
	if strings.Contains(out, "notifications ready") {
		t.Error("notifications reported ready without an encryption key")
	}
}

// With a key, the dispatcher starts and logs the key id — never the key.
func TestNotificationsEncryptionKeyIsNeverLogged(t *testing.T) {
	var logs syncBuffer
	base := bootServerWith(t, nil, debugLogger(&logs))
	client := authenticatedTestClient(t, base, "superadmin")

	for _, path := range []string{"/api/v1/notifications/channels", "/api/v1/auth/me"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	out := logs.String()
	if !strings.Contains(out, "notifications ready") || !strings.Contains(out, `"encryption_key_id"`) {
		t.Error("notifications did not report ready, or did not log the key id")
	}
	if strings.Contains(out, testNotificationsKey) {
		t.Fatal("the encryption key appeared in the server's logs")
	}
}
