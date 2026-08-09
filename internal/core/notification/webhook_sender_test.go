package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWebhookSenderDeliversAndSignsWithSecret(t *testing.T) {
	const secret = "shh"
	var gotBody []byte
	var gotSignature string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSignature = r.Header.Get(SignatureHeader)
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("content-type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	channel := Channel{Type: ChannelWebhook, Webhook: WebhookConfig{URL: srv.URL, Secret: secret}}
	d := Delivery{EventID: "evt-1", Trigger: TriggerAlertTransition, NodeID: "node-1", Severity: "critical", Title: "High CPU", EventTime: time.Now()}

	if err := (WebhookSender{Client: srv.Client()}).Send(context.Background(), channel, d); err != nil {
		t.Fatalf("send: %v", err)
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(gotBody)
	want := hex.EncodeToString(mac.Sum(nil))
	if gotSignature != want {
		t.Errorf("signature = %q, want %q", gotSignature, want)
	}

	var payload webhookPayload
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.EventID != "evt-1" || payload.NodeID != "node-1" || payload.Title != "High CPU" {
		t.Errorf("payload = %+v", payload)
	}
}

func TestWebhookSenderOmitsSignatureWithNoSecret(t *testing.T) {
	var gotSignature string
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSignature, sawHeader = r.Header.Get(SignatureHeader), r.Header.Get(SignatureHeader) != ""
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	channel := Channel{Type: ChannelWebhook, Webhook: WebhookConfig{URL: srv.URL}}
	if err := (WebhookSender{Client: srv.Client()}).Send(context.Background(), channel, Delivery{EventID: "evt-1"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if sawHeader {
		t.Errorf("expected no signature header with an unset secret, got %q", gotSignature)
	}
}

func TestWebhookSenderReturnsErrorOnNonSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	channel := Channel{Type: ChannelWebhook, Webhook: WebhookConfig{URL: srv.URL}}
	err := (WebhookSender{Client: srv.Client()}).Send(context.Background(), channel, Delivery{EventID: "evt-1"})
	if err == nil {
		t.Fatal("expected an error on a 500 response")
	}
}

func TestWebhookSenderReturnsErrorOnUnreachableDestination(t *testing.T) {
	channel := Channel{Type: ChannelWebhook, Webhook: WebhookConfig{URL: "http://127.0.0.1:1"}}
	sender := NewWebhookSender()
	sender.Client.Timeout = time.Second
	err := sender.Send(context.Background(), channel, Delivery{EventID: "evt-1"})
	if err == nil {
		t.Fatal("expected an error connecting to a closed port")
	}
}
