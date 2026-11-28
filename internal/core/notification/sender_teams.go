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
		return TeamsSecret{}, errs.New(errs.CodeInvalidArgument, "the teams channel secret is not valid JSON").WithOp(op).WithDetail("field", "url")
	}
	if s.URL == "" {
		return TeamsSecret{}, errs.New(errs.CodeInvalidArgument, "a teams channel secret needs a url").WithOp(op).WithDetail("field", "url")
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

// cardTimeFormat is how the card shows a delivery's time.
const cardTimeFormat = "2006-01-02 15:04 UTC"

// adaptiveCard builds a small, plain Adaptive Card 1.4: a kind-aware,
// coloured title, the facts the delivery carries (any empty one left
// out), then the rendered message.
//
// Delivery carries no separate "value" or "since" field — those live on
// the evaluator's [State], which a [Sender] never sees — so they reach the
// card only as far as the evaluator wrote them into the message.
func adaptiveCard(d Delivery) map[string]any {
	var facts []map[string]any
	addFact := func(title, value string) {
		if value != "" {
			facts = append(facts, map[string]any{"title": title, "value": value})
		}
	}
	addFact("Severity", string(d.Severity))
	addFact("Trigger", string(d.Trigger))
	if d.MonitorID != "" {
		for _, f := range monitorFacts(d) {
			addFact(f[0], f[1])
		}
	} else {
		addFact("Node", nodeLabel(d.NodeID, d.NodeName))
		addFact("Address", d.NodeAddress)
		addFact("Subject", d.Subject)
	}
	if !d.CreatedAt.IsZero() {
		addFact("Time", d.CreatedAt.UTC().Format(cardTimeFormat))
	}

	title := map[string]any{"type": "TextBlock", "text": cardTitle(d), "weight": "Bolder", "size": "Medium", "wrap": true}
	if c := cardTitleColor(d); c != "" {
		title["color"] = c
	}
	body := []map[string]any{title}
	if len(facts) > 0 {
		body = append(body, map[string]any{"type": "FactSet", "facts": facts})
	}
	body = append(body, map[string]any{"type": "TextBlock", "text": d.Message, "wrap": true})
	return map[string]any{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": "1.4",
		"body":    body,
	}
}

// monitorFacts are a monitor delivery's card facts, in order, as (title,
// value) pairs; an empty value is left out by the caller. Only the URL's
// host is ever shown.
func monitorFacts(d Delivery) [][2]string {
	name := d.MonitorName
	if name == "" {
		name = d.MonitorID
	}
	x := d.Details
	facts := [][2]string{{"Website", name}, {"Host", d.MonitorHost}}
	if x.Failure != "" {
		facts = append(facts, [2]string{"Failure", x.Failure})
	}
	if x.StatusCode != nil {
		facts = append(facts, [2]string{"Status code", fmt.Sprint(*x.StatusCode)})
	}
	if x.ResponseMS != nil {
		facts = append(facts, [2]string{"Response time", fmt.Sprintf("%d ms", *x.ResponseMS)})
	}
	if x.DownSince != nil {
		facts = append(facts, [2]string{"Down since", x.DownSince.UTC().Format(cardTimeFormat)})
	}
	if x.OutageSeconds != nil {
		facts = append(facts, [2]string{"Outage", cooldownText(*x.OutageSeconds)})
	}
	if x.CertDaysLeft != nil {
		facts = append(facts, [2]string{"Days remaining", fmt.Sprint(*x.CertDaysLeft)})
	}
	if x.CertExpiresAt != nil {
		facts = append(facts, [2]string{"Expires", x.CertExpiresAt.UTC().Format(cardTimeFormat)})
	}
	return facts
}

// nodeLabel names a node for a reader: its hostname with a short id, or
// the full id when no name is known.
func nodeLabel(nodeID, name string) string {
	if name == "" || nodeID == "" {
		return nodeID
	}
	const short = 8
	if len(nodeID) <= short {
		return fmt.Sprintf("%s (%s)", name, nodeID)
	}
	return fmt.Sprintf("%s (%s…)", name, nodeID[:short])
}

func cardTitle(d Delivery) string {
	switch d.Kind {
	case KindEscalated:
		return "Escalated: " + d.Title
	case KindRecovered:
		return "Recovered: " + d.Title
	case KindFlapping:
		return "Flapping: " + d.Title
	default:
		return d.Title
	}
}

// cardTitleColor is the title's Adaptive Card colour; "" keeps the default.
func cardTitleColor(d Delivery) string {
	switch d.Kind {
	case KindRecovered:
		return "Good"
	case KindTest, KindFlapping:
		return ""
	}
	switch d.Severity {
	case SeverityCritical:
		return "Attention"
	case SeverityWarning:
		return "Warning"
	}
	return ""
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
