package notification

import (
	"context"
	"errors"
	"time"
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
	// brought all of its own.
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
			EventID: n.EventID, Kind: n.Kind, ChannelType: d.channel.Type, Title: n.Title, Message: n.Message, CreatedAt: time.Now(),
		})
	}
	if err != nil {
		return SendTestOutcome{Error: SendErrorCategory(err)}
	}
	return SendTestOutcome{Delivered: true}
}

// PrepareDraftTest validates a draft channel without touching the network.
// When channelID names a saved channel of the same type, whatever the draft's
// secret leaves out is kept from the stored one, as in [PrepareUpdate]; the
// caller never sees it.
func (e *Engine) PrepareDraftTest(ctx context.Context, in ChannelInput, channelID string) (*DraftTest, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}

	d := &DraftTest{
		channel: Channel{Name: in.Name, Type: in.Type, Enabled: in.Enabled, MinSeverity: in.MinSeverity, Config: in.Config},
		sender:  e.senders[in.Type],
		target:  "type:" + string(in.Type),
	}
	var err error
	d.secret, d.ChannelID, err = completeSecret(ctx, e.channels, in, channelID)
	if errors.Is(err, ErrSecretUnreadable) {
		d.failed, d.target = err, "channel:"+d.ChannelID
		return d, nil
	}
	if err != nil {
		return nil, err
	}

	host, err := validateDestination(in.Type, in.Config, d.secret)
	if err != nil {
		return nil, err
	}
	if host != "" {
		d.target = "host:" + host
	}
	return d, nil
}
