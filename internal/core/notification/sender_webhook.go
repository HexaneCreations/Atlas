package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"text/template"

	"github.com/hexane/atlas/internal/platform/errs"
)

// defaultWebhookContentType is used when a WebhookConfig does not set one.
const defaultWebhookContentType = "application/json"

// WebhookConfig is a "webhook" channel's non-secret config (Channel.Config):
// only what never needs protecting — the body template and the content
// type to send it as. The destination URL and any additional headers are
// deliberately not here; see [WebhookSecret].
type WebhookConfig struct {
	// Template is a text/template body rendered with four variables —
	// msg, eventJSON, nodeJSON, and ruleJSON — each already a ready-to-embed
	// JSON fragment: msg is a JSON string literal (quotes included), the
	// other three are JSON object literals. A template writes
	// {"text": {{.msg}}, "event": {{.eventJSON}}} with no extra quoting or
	// escaping of its own.
	//
	// nodeJSON and ruleJSON are limited to what a [Delivery] actually
	// carries: {"node_id": "..."} and {"trigger": "..."}. The full Node
	// and Rule rows are not available at Send time (Sender.Send receives a
	// Delivery, not a joined Node or RuleSpec) — enriching these would mean
	// changing what the dispatcher passes to Send, which is step 8's
	// concern, not this sender's.
	Template    string
	ContentType string
}

// DecodeWebhookConfig reads a "webhook" channel's stored config.
func DecodeWebhookConfig(config map[string]any) (WebhookConfig, error) {
	const op = "notification.DecodeWebhookConfig"
	c := WebhookConfig{ContentType: defaultWebhookContentType}
	if tmpl, _ := config["template"].(string); tmpl != "" {
		c.Template = tmpl
	} else {
		return WebhookConfig{}, errs.New(errs.CodeInvalidArgument, "a webhook channel needs a template").WithOp(op).WithDetail("field", "template")
	}
	if ct, ok := config["content_type"].(string); ok && ct != "" {
		c.ContentType = ct
	}
	return c, nil
}

// Validate checks a WebhookConfig: the template's presence, size, and
// that it parses, and the content type's shape. It does not — cannot —
// check the destination or headers: those are [WebhookSecret]'s.
func (c WebhookConfig) Validate() error {
	const op = "notification.WebhookConfig.Validate"
	if len(c.Template) == 0 {
		return errs.New(errs.CodeInvalidArgument, "a template is required").WithOp(op).WithDetail("field", "template")
	}
	if len(c.Template) > MaxTemplateLength {
		return errs.New(errs.CodeInvalidArgument, "the template exceeds %d bytes", MaxTemplateLength).WithOp(op).WithDetail("field", "template")
	}
	if _, err := parseWebhookTemplate(c.Template); err != nil {
		return errs.New(errs.CodeInvalidArgument, "the template is not valid").WithOp(op).
			WithDetail("field", "template").WithDetail("reason", strings.TrimPrefix(err.Error(), "template: webhook:"))
	}
	if strings.ContainsAny(c.ContentType, "\r\n") {
		return errs.New(errs.CodeInvalidArgument, "content_type must not contain a newline").WithOp(op).WithDetail("field", "content_type")
	}
	return nil
}

// Masked is what a reader who can see this channel exists is shown for its
// non-secret config: the content type and the template's size, never the
// template body itself (which could legitimately quote a value an
// operator considers sensitive even though it carries no credential this
// package knows about).
func (c WebhookConfig) Masked() map[string]any {
	return map[string]any{
		"content_type":  c.ContentType,
		"template_size": len(c.Template),
	}
}

// parseWebhookTemplate parses a webhook body template with no functions
// beyond text/template's own built-ins (and, or, printf, …) — this
// package adds none of its own, so a template cannot reach outside the
// four variables it is given — and "missingkey=error", so a template
// referencing anything other than msg, eventJSON, nodeJSON, or ruleJSON
// fails loudly at render time instead of silently rendering Go's
// "<no value>" into what is otherwise meant to be valid JSON.
func parseWebhookTemplate(body string) (*template.Template, error) {
	return template.New("webhook").Option("missingkey=error").Parse(body)
}

// WebhookSecret is a "webhook" channel's sealed secret: the destination
// URL and any additional headers. Both live here, not in Channel.Config,
// because either can carry the credential — Slack and Discord both embed
// a bearer-style token in the URL *path*, not a query parameter, and a
// header commonly carries Authorization.
type WebhookSecret struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// DecodeWebhookSecret reads a "webhook" channel's decrypted secret.
func DecodeWebhookSecret(secret string) (WebhookSecret, error) {
	const op = "notification.DecodeWebhookSecret"
	var s WebhookSecret
	if err := json.Unmarshal([]byte(secret), &s); err != nil {
		return WebhookSecret{}, errs.New(errs.CodeInvalidArgument, "the webhook channel secret is not valid JSON").WithOp(op).WithDetail("field", "url")
	}
	if s.URL == "" {
		return WebhookSecret{}, errs.New(errs.CodeInvalidArgument, "a webhook channel secret needs a url").WithOp(op).WithDetail("field", "url")
	}
	return s, nil
}

// Validate checks a WebhookSecret: the destination url (https, length cap,
// no userinfo — no host allowlist, unlike teams) and the headers (count,
// size, deny-list, no CR/LF).
func (s WebhookSecret) Validate() error {
	const op = "notification.WebhookSecret.Validate"
	if err := validateDestinationURL(s.URL, nil, op); err != nil {
		return err
	}
	return validateHeaders(s.Headers, op)
}

// Masked is what a reader who can see this channel exists, but must not
// see where it points or what it sends, is shown: the host, and header
// names without their values.
func (s WebhookSecret) Masked() map[string]any {
	return map[string]any{
		"url":          maskURL(s.URL),
		"header_names": maskedHeaderNames(s.Headers),
	}
}

// webhookEvent is the eventJSON template variable: the notification event
// itself, as far as a Delivery carries it.
type webhookEvent struct {
	EventID  string `json:"event_id"`
	Kind     string `json:"kind"`
	Trigger  string `json:"trigger"`
	NodeID   string `json:"node_id,omitempty"`
	Subject  string `json:"subject,omitempty"`
	Severity string `json:"severity,omitempty"`
	Title    string `json:"title"`
	Message  string `json:"message"`
}

// jsonFragment marshals v and returns it as a string, ready to embed
// literally in a template's output (a JSON string literal for a Go
// string, an object literal for a struct or map). Marshal failure on these
// fixed, already-validated-by-the-type-system shapes is not something the
// caller needs to handle differently from any other internal error.
func jsonFragment(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WebhookSender delivers a notification as a user-templated JSON POST.
type WebhookSender struct {
	client *http.Client
}

// NewWebhookSender builds a WebhookSender with the SSRF-guarded client
// every HTTP-based sender uses.
func NewWebhookSender() *WebhookSender {
	return &WebhookSender{client: newGuardedClient(DefaultSendTimeout)}
}

// Send implements [Sender]. secret is this call's one use of the
// channel's decrypted URL and headers: they are read into webhookSecret
// below and nothing derived from them — not the URL, not a header value,
// not even in an error — leaves this function except inside the request
// Go's own http.Client sends.
func (s *WebhookSender) Send(ctx context.Context, channel Channel, secret string, d Delivery) error {
	cfg, err := DecodeWebhookConfig(channel.Config)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	webhookSecret, err := DecodeWebhookSecret(secret)
	if err != nil {
		return err
	}
	if err := webhookSecret.Validate(); err != nil {
		return err
	}

	tmpl, err := parseWebhookTemplate(cfg.Template)
	if err != nil {
		return fmt.Errorf("parse webhook template: %w", err)
	}

	msg, err := jsonFragment(d.Message)
	if err != nil {
		return fmt.Errorf("encode msg: %w", err)
	}
	eventJSON, err := jsonFragment(webhookEvent{
		EventID: d.EventID, Kind: string(d.Kind), Trigger: string(d.Trigger), NodeID: d.NodeID,
		Subject: d.Subject, Severity: string(d.Severity), Title: d.Title, Message: d.Message,
	})
	if err != nil {
		return fmt.Errorf("encode eventJSON: %w", err)
	}
	nodeJSON, err := jsonFragment(map[string]string{"node_id": d.NodeID})
	if err != nil {
		return fmt.Errorf("encode nodeJSON: %w", err)
	}
	ruleJSON, err := jsonFragment(map[string]string{"trigger": string(d.Trigger)})
	if err != nil {
		return fmt.Errorf("encode ruleJSON: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]string{
		"msg": msg, "eventJSON": eventJSON, "nodeJSON": nodeJSON, "ruleJSON": ruleJSON,
	}); err != nil {
		return &SendError{msg: "render webhook template: " + err.Error(), category: categoryTemplate}
	}
	// Checked after rendering, against the rendered size — a template this
	// package already bounded at MaxTemplateLength can still produce a
	// body far larger than its own source once msg/eventJSON/nodeJSON/
	// ruleJSON are substituted in, and it is the rendered body that is
	// actually sent.
	if buf.Len() > DefaultRequestBodyLimit {
		return ErrRequestTooLarge
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookSecret.URL, bytes.NewReader(buf.Bytes()))
	if err != nil {
		return wrapSendError("build webhook request", err)
	}
	req.Header.Set("Content-Type", cfg.ContentType)
	for name, value := range webhookSecret.Headers {
		req.Header.Set(name, value)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return wrapSendError("deliver webhook", err)
	}
	snippet := readLimitedResponse(resp, DefaultResponseBodyLimit)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return ErrRedirectRefused
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError("webhook destination", resp.StatusCode, snippet)
	}
	return nil
}

var _ Sender = (*WebhookSender)(nil)
