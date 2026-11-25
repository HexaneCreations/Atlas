package notification

import "context"

// Sender delivers one delivery record to one channel. Implementations are
// per [ChannelType] and register under their own type without changing
// [Engine].
//
// secret is the channel's decrypted secret — for "teams" and "webhook",
// a JSON document carrying whatever must never sit in Channel.Config:
// the destination URL (a Teams Workflow's sig= or a Slack/Discord
// webhook's path-embedded token is the credential) and, for "webhook",
// any additional headers (Authorization in particular). [Engine] resolves
// it through the channel store immediately before calling Send, so it
// lives only for the one call — a Sender must not log it, return it, wrap
// it into an error, or store it anywhere; let it go out of scope when Send
// returns.
type Sender interface {
	Send(ctx context.Context, channel Channel, secret string, d Delivery) error
}
