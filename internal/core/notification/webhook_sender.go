package notification

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// WebhookSender delivers a notification as a signed JSON POST.
type WebhookSender struct {
	Client *http.Client
}

// NewWebhookSender builds a WebhookSender with a bounded request timeout —
// a hung destination must not hold the dispatch loop open indefinitely.
func NewWebhookSender() WebhookSender {
	return WebhookSender{Client: &http.Client{Timeout: 10 * time.Second}}
}

// webhookPayload is the JSON body posted to the destination URL.
type webhookPayload struct {
	EventID  string    `json:"event_id"`
	Trigger  Trigger   `json:"trigger"`
	NodeID   string    `json:"node_id,omitempty"`
	Severity string    `json:"severity,omitempty"`
	Title    string    `json:"title,omitempty"`
	Message  string    `json:"message,omitempty"`
	Time     time.Time `json:"time"`
}

// SignatureHeader carries the HMAC-SHA256 hex signature of the request
// body, computed with the channel's configured secret, when one is set.
const SignatureHeader = "X-Atlas-Signature"

func (s WebhookSender) Send(ctx context.Context, channel Channel, d Delivery) error {
	body, err := json.Marshal(webhookPayload{
		EventID: d.EventID, Trigger: d.Trigger, NodeID: d.NodeID,
		Severity: d.Severity, Title: d.Title, Message: d.Message, Time: d.EventTime,
	})
	if err != nil {
		return fmt.Errorf("encode webhook payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, channel.Webhook.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build webhook request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if channel.Webhook.Secret != "" {
		mac := hmac.New(sha256.New, []byte(channel.Webhook.Secret))
		mac.Write(body)
		req.Header.Set(SignatureHeader, hex.EncodeToString(mac.Sum(nil)))
	}

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook destination returned status %d", resp.StatusCode)
	}
	return nil
}

var _ Sender = WebhookSender{}
