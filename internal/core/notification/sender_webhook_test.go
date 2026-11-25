package notification

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// webhookSecretJSON builds the sealed-secret JSON a "webhook" channel's
// decrypted secret_ciphertext decodes to.
func webhookSecretJSON(t *testing.T, url string, headers map[string]string) string {
	t.Helper()
	b, err := json.Marshal(struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}{URL: url, Headers: headers})
	if err != nil {
		t.Fatalf("marshal secret fixture: %v", err)
	}
	return string(b)
}

func TestWebhookSenderRendersTemplateAndHeaders(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	var gotHeaders http.Header
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &WebhookSender{client: srv.Client()}
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{
		"template": `{"msg": {{.msg}}, "event": {{.eventJSON}}, "node": {{.nodeJSON}}, "rule": {{.ruleJSON}}}`,
	}}
	secret := webhookSecretJSON(t, srv.URL, map[string]string{"Authorization": "Bearer top-secret-abc", "X-Custom": "yes"})
	d := Delivery{
		EventID: "evt-9", Kind: KindFiring, Trigger: TriggerRAMPct, NodeID: "node-3", Subject: "mem",
		Severity: SeverityWarning, Title: "ram high", Message: "ram at 92%",
	}

	if err := sender.Send(context.Background(), channel, secret, d); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if ct := gotHeaders.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json (the default)", ct)
	}
	if auth := gotHeaders.Get("Authorization"); auth != "Bearer top-secret-abc" {
		t.Errorf("Authorization = %q, want the configured value (it is allowed, just never logged)", auth)
	}
	if custom := gotHeaders.Get("X-Custom"); custom != "yes" {
		t.Errorf("X-Custom = %q, want %q", custom, "yes")
	}

	var decoded struct {
		Msg   string         `json:"msg"`
		Event map[string]any `json:"event"`
		Node  map[string]any `json:"node"`
		Rule  map[string]any `json:"rule"`
	}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("the rendered body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	if decoded.Msg != d.Message {
		t.Errorf("msg = %q, want %q", decoded.Msg, d.Message)
	}
	if decoded.Event["event_id"] != d.EventID || decoded.Event["trigger"] != string(d.Trigger) {
		t.Errorf("event = %v, missing expected fields", decoded.Event)
	}
	if decoded.Node["node_id"] != d.NodeID {
		t.Errorf("node = %v, want node_id %q", decoded.Node, d.NodeID)
	}
	if decoded.Rule["trigger"] != string(d.Trigger) {
		t.Errorf("rule = %v, want trigger %q", decoded.Rule, d.Trigger)
	}
}

// TestWebhookSenderEscapesMessageSafely proves a message containing
// characters that would otherwise break the JSON produced by a naive
// template (a quote, a backslash, a newline) still renders valid JSON,
// since msg is provided already as a complete, escaped JSON string
// literal rather than requiring the template author to escape it.
func TestWebhookSenderEscapesMessageSafely(t *testing.T) {
	t.Parallel()
	var gotBody []byte
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sender := &WebhookSender{client: srv.Client()}
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{"template": `{"text": {{.msg}}}`}}
	secret := webhookSecretJSON(t, srv.URL, nil)
	tricky := `disk at 95% on "node-1"\nbackslash: \ and a tab:	end`
	d := Delivery{EventID: "e", Trigger: TriggerDiskPct, Title: "t", Message: tricky}

	if err := sender.Send(context.Background(), channel, secret, d); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("rendered body is not valid JSON: %v\nbody: %s", err, gotBody)
	}
	if decoded.Text != tricky {
		t.Errorf("text = %q, want %q", decoded.Text, tricky)
	}
}

// TestWebhookSenderRendersMissingVariableLoudly proves the template engine
// uses missingkey=error: a template referencing anything other than msg,
// eventJSON, nodeJSON, or ruleJSON fails the send rather than silently
// posting Go's "<no value>" as part of the body.
func TestWebhookSenderRendersMissingVariableLoudly(t *testing.T) {
	t.Parallel()
	sender := NewWebhookSender()
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{"template": `{"x": {{.notAVariable}}}`}}
	secret := webhookSecretJSON(t, "https://hooks.example.test/in", nil)

	err := sender.Send(context.Background(), channel, secret, Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected a template referencing an unknown variable to fail the send")
	}
}

func TestWebhookSenderRejectsDeniedHeaderAtSendTime(t *testing.T) {
	t.Parallel()
	sender := NewWebhookSender()
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{"template": `{{.msg}}`}}
	secret := webhookSecretJSON(t, "https://hooks.example.test/in", map[string]string{"Cookie": "a=b"})
	err := sender.Send(context.Background(), channel, secret, Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected a denied header to be rejected at send time, not just at config-creation time")
	}
}

func TestWebhookSenderRejectsOversizeRenderedBody(t *testing.T) {
	t.Parallel()
	sender := NewWebhookSender()
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{
		"template": `{"msg": {{.msg}}, "pad": "` + strings.Repeat("a", DefaultRequestBodyLimit) + `"}`,
	}}
	secret := webhookSecretJSON(t, "https://hooks.example.test/in", nil)
	err := sender.Send(context.Background(), channel, secret, Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected the oversize rendered body to be rejected before sending")
	}
}

func TestWebhookSenderTreatsRedirectAsFailure(t *testing.T) {
	t.Parallel()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect target was reached")
	}))
	defer target.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer srv.Close()

	client := srv.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	sender := &WebhookSender{client: client}
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{"template": `{{.msg}}`}}
	secret := webhookSecretJSON(t, srv.URL, nil)
	err := sender.Send(context.Background(), channel, secret, Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected a redirect response to be treated as a failed delivery")
	}
}

// TestWebhookSenderNeverPutsTheURLInAnError proves item 2 for the webhook
// sender specifically: Slack and Discord both encode their credential in
// the URL *path*, not a query parameter, so this must hold even though
// there is no sig= for a naive query-only redaction to catch. The
// destination is loopback, refused by the SSRF guard deterministically and
// without touching the real network or a real Slack-shaped host (see
// httpclient_test.go for the guard's own tests).
func TestWebhookSenderNeverPutsTheURLInAnError(t *testing.T) {
	t.Parallel()
	sender := NewWebhookSender()
	channel := Channel{Type: ChannelWebhook, Config: map[string]any{"template": `{{.msg}}`}}
	const slackToken = "T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"
	secret := webhookSecretJSON(t, "https://127.0.0.1:1/services/"+slackToken, nil)

	err := sender.Send(context.Background(), channel, secret, Delivery{Title: "t", Message: "m"})
	if err == nil {
		t.Fatal("expected the destination to fail (loopback is refused by the SSRF guard before any real dial)")
	}
	if strings.Contains(err.Error(), slackToken) {
		t.Errorf("error leaked the url: %v", err)
	}
}
