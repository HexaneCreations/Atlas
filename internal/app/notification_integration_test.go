//go:build integration

// The notification pipeline is wired entirely through the alert engine's
// OnTransition hook (see internal/app/notification.go and app.go's
// onTransition closure) — this is the test that proves a real alert
// transition, evaluated by the real alert engine against real collected
// metrics, ends in a real signed webhook delivery, durably recorded.
package app_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func postJSON(t *testing.T, client *http.Client, url, body string) (int, map[string]any) {
	t.Helper()
	resp, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &decoded); err != nil {
			t.Fatalf("decode %s (%q): %v", url, respBody, err)
		}
	}
	return resp.StatusCode, decoded
}

// TestAlertTransitionDeliversARealSignedWebhook exercises the full path: a
// threshold alert rule evaluated by the real, running alert engine against
// real collected host metrics, firing into the real notification
// dispatcher, delivered as a real HMAC-signed HTTP POST to a real server.
//
// This runs the alert engine's default 30s evaluation tick, so it is
// deliberately slow — the cost of proving the wiring end to end rather
// than through a fake.
func TestAlertTransitionDeliversARealSignedWebhook(t *testing.T) {
	if os.Getenv(testDatabaseURLEnv) == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)
	// Registering a notification channel and an alert rule are both
	// fleet.write actions now — see docs/adr/0013-human-user-authentication-and-authorization.md
	// and migrations/0013_fleet_write_permission.sql.
	client := authenticatedTestClient(t, base, "operator")

	const secret = "integration-secret"
	var (
		mu        sync.Mutex
		received  []byte
		signature string
	)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		received = body
		signature = r.Header.Get("X-Atlas-Signature")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()

	// Register the webhook channel.
	channelBody := `{"name":"it-webhook","type":"webhook","webhook_url":"` + webhook.URL + `","webhook_secret":"` + secret + `"}`
	status, _ := postJSON(t, client, base+"/api/v1/notifications/channels", channelBody)
	if status != http.StatusCreated {
		t.Fatalf("create channel status = %d", status)
	}

	// A threshold rule guaranteed to be true the moment it is evaluated:
	// CPU usage is always >= 0, so "> -1" always fires. No node scope, so
	// it evaluates against whatever node is actually reporting.
	ruleBody := `{"name":"it-always-fires","kind":"threshold","severity":"critical",
		"metric":"system.cpu.usage","comparison":"gt","threshold":-1,"for_seconds":0}`
	status, _ = postJSON(t, client, base+"/api/v1/alerts/rules", ruleBody)
	if status != http.StatusCreated {
		t.Fatalf("create rule status = %d", status)
	}

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		got := received
		mu.Unlock()
		if got != nil {
			break
		}
		time.Sleep(time.Second)
	}

	mu.Lock()
	body, sig := received, signature
	mu.Unlock()
	if body == nil {
		t.Fatal("webhook never received a delivery within the deadline")
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	wantSig := hex.EncodeToString(mac.Sum(nil))
	if sig != wantSig {
		t.Errorf("signature = %q, want %q", sig, wantSig)
	}

	var payload struct {
		Trigger  string `json:"trigger"`
		Severity string `json:"severity"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Trigger != "alert_transition" {
		t.Errorf("trigger = %q, want alert_transition", payload.Trigger)
	}
	if payload.Severity != "critical" {
		t.Errorf("severity = %q, want critical", payload.Severity)
	}

	// The delivery is durably recorded as delivered.
	resp, err := http.Get(base + "/api/v1/notifications/deliveries?status=delivered")
	if err != nil {
		t.Fatalf("get deliveries: %v", err)
	}
	defer resp.Body.Close()
	var list struct {
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode deliveries: %v", err)
	}
	if list.Total < 1 {
		t.Fatal("expected at least one delivered delivery recorded")
	}
}
