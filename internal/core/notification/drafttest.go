package notification

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/hexane/atlas/internal/platform/errs"
)

// DraftTest is a validated, not yet sent, test of a channel that may never
// be saved. It holds the secret, so its fields stay private to this package.
type DraftTest struct {
	channel Channel
	secret  string
	sender  Sender
	// failed is a stored secret that could not be read: reported by Send,
	// the way a saved channel's test reports it.
	failed error
	target string
	// ChannelID is the saved channel whose secret is used, "" when the draft
	// brought its own.
	ChannelID string
}

// Target is what the rate limiter counts the test against: the destination
// host, or the channel type when there is none.
func (d *DraftTest) Target() string { return d.target }

// Send sends the test. Nothing is stored and nothing is logged.
func (d *DraftTest) Send(ctx context.Context) SendTestOutcome {
	err := d.failed
	if err == nil && d.sender == nil {
		err = noSenderError(d.channel.Type)
	}
	if err == nil {
		n := testNotification(d.channel.Name)
		err = d.sender.Send(ctx, d.channel, d.secret, Delivery{
			EventID: n.EventID, Kind: n.Kind, ChannelType: d.channel.Type, Title: n.Title, Message: n.Message,
		})
	}
	if err != nil {
		return SendTestOutcome{Error: SendErrorCategory(err)}
	}
	return SendTestOutcome{Delivered: true}
}

// PrepareDraftTest validates a draft channel without touching the network.
// When the draft has no secret and channelID names a saved channel of the
// same type, that channel's stored secret is used; the caller never sees it.
//
// Every error's text is fixed: a rejected request is logged, and must not
// carry the draft's URL or headers into the log.
func (e *Engine) PrepareDraftTest(ctx context.Context, in ChannelInput, channelID string) (*DraftTest, error) {
	const op = "notification.Engine.PrepareDraftTest"
	if err := in.Validate(); err != nil {
		return nil, err
	}

	d := &DraftTest{
		channel: Channel{Name: in.Name, Type: in.Type, Enabled: in.Enabled, MinSeverity: in.MinSeverity, Config: in.Config},
		sender:  e.senders[in.Type],
		target:  "type:" + string(in.Type),
	}
	if in.Secret != nil {
		d.secret = *in.Secret
	}
	if strings.TrimSpace(d.secret) == "" && channelID != "" {
		saved, err := e.channels.GetChannel(ctx, channelID)
		if err != nil {
			return nil, err
		}
		// A stored secret is only ever used as the kind of channel it was
		// saved for.
		if saved.Type != in.Type {
			return nil, errs.New(errs.CodeInvalidArgument, "channel_id names a channel of another type").WithOp(op)
		}
		d.ChannelID = saved.ID
		d.secret, err = e.channels.ChannelSecret(ctx, saved.ID)
		switch {
		case errors.Is(err, ErrSecretUnreadable):
			d.failed, d.target = err, "channel:"+saved.ID
			return d, nil
		case errs.CodeOf(err) == errs.CodeFailedPrecondition:
			return nil, errs.New(errs.CodeInvalidArgument, "the channel has no stored secret; supply one").WithOp(op)
		case err != nil:
			return nil, err
		}
	}

	destination, err := validateDraft(in.Type, in.Config, d.secret)
	if err != nil {
		return nil, err
	}
	if destination != "" {
		host, err := publicHost(destination)
		if err != nil {
			return nil, err
		}
		d.target = "host:" + host
	}
	return d, nil
}

// validateDraft runs the sender's own validation for the channel's type and
// returns the destination URL, "" for a type with none.
func validateDraft(t ChannelType, config map[string]any, secret string) (string, error) {
	const op = "notification.validateDraft"
	if t != ChannelTeams && t != ChannelWebhook {
		return "", nil
	}
	if strings.TrimSpace(secret) == "" {
		return "", errs.New(errs.CodeInvalidArgument, "a secret is required").WithOp(op)
	}

	if t == ChannelTeams {
		s, err := DecodeTeamsSecret(secret)
		if err != nil {
			return "", err
		}
		if err := validateDestinationURL(s.URL, nil, op); err != nil {
			return "", err
		}
		if s.Validate() != nil {
			return "", errs.New(errs.CodeInvalidArgument, "the destination host is not an allowed Teams host (%s)",
				strings.Join(TeamsAllowedHostSuffixes, ", ")).WithOp(op)
		}
		return s.URL, nil
	}

	c, err := DecodeWebhookConfig(config)
	if err != nil {
		return "", err
	}
	if err := c.Validate(); err != nil {
		return "", err
	}
	s, err := DecodeWebhookSecret(secret)
	if err != nil {
		return "", err
	}
	if err := validateDestinationURL(s.URL, nil, op); err != nil {
		return "", err
	}
	if s.Validate() != nil {
		return "", errs.New(errs.CodeInvalidArgument,
			"a custom header is not allowed: check the number, name, length and characters of the headers").WithOp(op)
	}
	return s.URL, nil
}

// publicHost returns rawURL's host, refusing one that is visibly not public:
// a non-public IP literal, localhost, or a numeric form a resolver may read
// as an IPv4 address (127.1, 0x7f000001). It resolves nothing; a name that
// resolves to a non-public address is refused when it is dialed.
func publicHost(rawURL string) (string, error) {
	refused := errs.New(errs.CodeInvalidArgument, "%s", categoryUnsafe).WithOp("notification.publicHost")
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", refused
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", refused
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if !isSafePublicAddr(net.IP(addr.WithZone("").AsSlice())) {
			return "", refused
		}
		return host, nil
	}
	last := host[strings.LastIndex(host, ".")+1:]
	if strings.HasPrefix(last, "0x") || strings.Trim(last, "0123456789") == "" {
		return "", refused
	}
	return host, nil
}
