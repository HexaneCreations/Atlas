package notification

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hexane/atlas/internal/platform/errs"
)

// TeamsSecret is a "teams" channel's sealed secret, decoded: just the
// destination URL. The URL is the whole credential this channel type
// needs — a Teams Workflow trusts whoever holds its URL, and its sig=
// query parameter *is* that trust — so it lives only in the sealed
// secret_ciphertext, opened through Repository.ChannelSecret, never in
// Channel.Config: config is stored as plain jsonb, and a "teams" channel's
// config carries no fields at all.
type TeamsSecret struct {
	URL string `json:"url"`
}

// DecodeTeamsSecret reads a "teams" channel's decrypted secret.
func DecodeTeamsSecret(secret string) (TeamsSecret, error) {
	const op = "notification.DecodeTeamsSecret"
	var s TeamsSecret
	if err := json.Unmarshal([]byte(secret), &s); err != nil {
		return TeamsSecret{}, errs.New(errs.CodeInvalidArgument, "the teams channel secret is not valid JSON").WithOp(op)
	}
	if s.URL == "" {
		return TeamsSecret{}, errs.New(errs.CodeInvalidArgument, "a teams channel secret needs a url").WithOp(op)
	}
	return s, nil
}

// Validate checks a TeamsSecret: https, within the length cap, no
// userinfo, and a host ending in one of [TeamsAllowedHostSuffixes].
func (s TeamsSecret) Validate() error {
	return validateDestinationURL(s.URL, TeamsAllowedHostSuffixes, "notification.TeamsSecret.Validate")
}

// Masked is what a reader who can see this channel exists, but must not
// see where it points, is shown: the host only.
func (s TeamsSecret) Masked() map[string]any {
	return map[string]any{"url": maskURL(s.URL)}
}

// adaptiveCard builds the small, plain card the brief asks for: severity,
// trigger, node and subject as facts, then the delivery's own rendered
// title and message as the card body.
//
// Delivery carries no separate "value" or "since" field — those live on
// the evaluator's [State] (Value, ConditionSince), which a [Sender] never
// sees, only the rendered [Delivery.Title] and [Delivery.Message] it
// produced from them. So "value and since-when" reach the card only
// insofar as whatever builds the Notification already wrote them into
// Title or Message — this sender does not fabricate a value fact from
// data it was not given.
func adaptiveCard(d Delivery) map[string]any {
	facts := []map[string]any{
		{"title": "Severity", "value": string(d.Severity)},
		{"title": "Trigger", "value": string(d.Trigger)},
		{"title": "Node", "value": d.NodeID},
	}
	if d.Subject != "" {
		facts = append(facts, map[string]any{"title": "Subject", "value": d.Subject})
	}
	body := []map[string]any{
		{"type": "TextBlock", "text": d.Title, "weight": "Bolder", "size": "Medium", "wrap": true},
		{"type": "FactSet", "facts": facts},
		{"type": "TextBlock", "text": d.Message, "wrap": true},
	}
	return map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
}

// teamsEnvelope is the message Teams Workflows expects a webhook POST to
// carry: a single Adaptive Card attachment.
type teamsEnvelope struct {
	Type        string            `json:"type"`
	Attachments []teamsAttachment `json:"attachments"`
}

type teamsAttachment struct {
	ContentType string         `json:"contentType"`
	ContentURL  *string        `json:"contentUrl"`
	Content     map[string]any `json:"content"`
}

// TeamsSender delivers a notification as a Teams Workflows Adaptive Card.
type TeamsSender struct {
	client *http.Client
}

// NewTeamsSender builds a TeamsSender with the SSRF-guarded client every
// HTTP-based sender uses.
func NewTeamsSender() *TeamsSender {
	return &TeamsSender{client: newGuardedClient(DefaultSendTimeout)}
}

// Send implements [Sender]. secret is this call's one use of the
// channel's decrypted URL: it is read into teamsSecret below and nothing
// derived from it — not the URL, not even in an error — leaves this
// function except inside the request Go's own http.Client sends.
func (s *TeamsSender) Send(ctx context.Context, channel Channel, secret string, d Delivery) error {
	teamsSecret, err := DecodeTeamsSecret(secret)
	if err != nil {
		return err
	}
	if err := teamsSecret.Validate(); err != nil {
		return err
	}

	body, err := json.Marshal(teamsEnvelope{
		Type: "message",
		Attachments: []teamsAttachment{{
			ContentType: "application/vnd.microsoft.card.adaptive",
			ContentURL:  nil,
			Content:     adaptiveCard(d),
		}},
	})
	if err != nil {
		return fmt.Errorf("encode teams payload: %w", err)
	}
	if len(body) > DefaultRequestBodyLimit {
		return ErrRequestTooLarge
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teamsSecret.URL, bytes.NewReader(body))
	if err != nil {
		return wrapSendError("build teams request", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return wrapSendError("deliver to teams", err)
	}
	snippet := readLimitedResponse(resp, DefaultResponseBodyLimit)

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return ErrRedirectRefused
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return statusError("teams destination", resp.StatusCode, snippet)
	}
	return nil
}

var _ Sender = (*TeamsSender)(nil)
