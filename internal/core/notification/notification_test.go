package notification

import "testing"

func TestChannelValidateRequiresName(t *testing.T) {
	c := Channel{Type: ChannelWebhook, Webhook: WebhookConfig{URL: "http://example.invalid"}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected a validation error with no name")
	}
}

func TestChannelValidateRequiresWebhookURL(t *testing.T) {
	c := Channel{Name: "x", Type: ChannelWebhook}
	if err := c.Validate(); err == nil {
		t.Fatal("expected a validation error with no webhook url")
	}
}

func TestChannelValidateRejectsUnknownType(t *testing.T) {
	c := Channel{Name: "x", Type: "slack"}
	if err := c.Validate(); err == nil {
		t.Fatal("expected a validation error for an unsupported channel type")
	}
}

func TestChannelValidateAcceptsAWellFormedWebhook(t *testing.T) {
	c := Channel{Name: "x", Type: ChannelWebhook, Webhook: WebhookConfig{URL: "http://example.invalid"}}
	if err := c.Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}
